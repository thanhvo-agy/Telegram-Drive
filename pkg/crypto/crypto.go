package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

var (
	// MagicHeader is the legacy TeleClone Crypt Version 1 header.
	MagicHeader = []byte("TGC1")
	// MagicHeaderV1 is an alias for MagicHeader.
	MagicHeaderV1 = MagicHeader
	// MagicHeaderV2 is the TeleClone Crypt Version 2 header with compression & AAD metadata.
	MagicHeaderV2 = []byte("TGC2")

	ErrInvalidHeader    = errors.New("invalid encryption magic header")
	ErrDecryptionFailed = errors.New("decryption failed: incorrect password or corrupted data")
	ErrDecompressFailed = errors.New("decompression failed: corrupted data")
)

const (
	SaltSize    = 16
	NonceSize   = 12
	KeySize     = 32 // AES-256
	PBKDF2Iters = 60000
	GCMTagSize  = 16

	HeaderV1Size = 4 // Magic [4B]
	HeaderV2Size = 9 // Magic [4B] + Flags [1B] + OrigSz [4B]

	MinContainerV1Size = HeaderV1Size + SaltSize + NonceSize + GCMTagSize // 4 + 16 + 12 + 16 = 48
	MinContainerV2Size = HeaderV2Size + SaltSize + NonceSize + GCMTagSize // 9 + 16 + 12 + 16 = 53
)

// DeriveKey derives a 32-byte AES-256 key from a passphrase and salt using PBKDF2-HMAC-SHA256.
func DeriveKey(passphrase string, salt []byte) []byte {
	return pbkdf2SHA256([]byte(passphrase), salt, PBKDF2Iters, KeySize)
}

// pbkdf2SHA256 implements RFC 2898 PBKDF2 using HMAC-SHA256 with pure Go standard library.
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen

	var buf [4]byte
	dk := make([]byte, 0, numBlocks*hashLen)
	u := make([]byte, hashLen)
	ti := make([]byte, hashLen)

	for block := 1; block <= numBlocks; block++ {
		// U_1 = PRF(password, salt || INT_32_BE(block))
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(buf[:], uint32(block))
		prf.Write(buf[:])
		u = prf.Sum(u[:0])
		copy(ti, u)

		// U_2 .. U_c
		for i := 2; i <= iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for k := 0; k < hashLen; k++ {
				ti[k] ^= u[k]
			}
		}
		dk = append(dk, ti...)
	}

	return dk[:keyLen]
}

// EncryptChunkWithCompression encrypts data into a TGC2 container with optional Zstandard compression.
// Container layout: [4B Magic "TGC2"][1B Flags][4B OrigSz uint32 BE][16B Salt][12B Nonce][Ciphertext + 16B GCM Tag]
// AAD: The first 9 bytes (Magic + Flags + OrigSz) are passed as Additional Authenticated Data to AES-GCM.
func EncryptChunkWithCompression(plaintext []byte, passphrase string, compType CompressionType) ([]byte, error) {
	if uint64(len(plaintext)) > uint64(math.MaxUint32) {
		return nil, errors.New("plaintext too large for uint32 chunk size")
	}

	var (
		payload   []byte
		finalFlag CompressionType
	)

	switch compType {
	case CompressionNone:
		finalFlag = CompressionNone
		payload = plaintext

	case CompressionZstd:
		if len(plaintext) == 0 {
			finalFlag = CompressionNone
			payload = plaintext
		} else {
			compressed, err := CompressData(plaintext)
			if err != nil {
				return nil, fmt.Errorf("compress chunk: %w", err)
			}
			// Incompressible bypass: If compressed size >= raw size, bypass compression
			// to avoid data inflation on media files (JPEG, PNG, MP4, etc.)
			if len(compressed) >= len(plaintext) {
				finalFlag = CompressionNone
				payload = plaintext
			} else {
				finalFlag = CompressionZstd
				payload = compressed
			}
		}

	default:
		return nil, fmt.Errorf("unsupported compression type: %d", compType)
	}

	salt := make([]byte, SaltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("generate salt: %w", err)
	}

	key := DeriveKey(passphrase, salt)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create gcm: %w", err)
	}

	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	// 9-byte AAD header: [4B Magic TGC2][1B Flags][4B OrigSz uint32 BE]
	aad := make([]byte, HeaderV2Size)
	copy(aad[0:4], MagicHeaderV2)
	aad[4] = byte(finalFlag)
	binary.BigEndian.PutUint32(aad[5:9], uint32(len(plaintext)))

	// Encrypt payload with GCM Seal using aad as Additional Authenticated Data
	ciphertext := gcm.Seal(nil, nonce, payload, aad)

	// Assemble final TGC2 container
	out := make([]byte, 0, HeaderV2Size+SaltSize+NonceSize+len(ciphertext))
	out = append(out, aad...)
	out = append(out, salt...)
	out = append(out, nonce...)
	out = append(out, ciphertext...)

	return out, nil
}

// EncryptChunk encrypts raw data using AES-256-GCM and Zstandard compression (TGC2 container).
// Falls back to uncompressed mode automatically if data is incompressible.
func EncryptChunk(plaintext []byte, passphrase string) ([]byte, error) {
	return EncryptChunkWithCompression(plaintext, passphrase, CompressionZstd)
}

// DecryptChunk authenticates and decrypts a chunk generated by EncryptChunk or EncryptChunkWithCompression.
// Transparently handles both legacy TGC1 and modern TGC2 containers.
func DecryptChunk(data []byte, passphrase string) ([]byte, error) {
	if len(data) < 4 {
		return nil, ErrInvalidHeader
	}

	if bytes.Equal(data[:4], MagicHeaderV2) {
		// TGC2 Container
		if len(data) < MinContainerV2Size {
			return nil, ErrDecryptionFailed
		}

		aad := data[:HeaderV2Size]
		flag := CompressionType(aad[4])
		origSz := binary.BigEndian.Uint32(aad[5:9])

		salt := data[HeaderV2Size : HeaderV2Size+SaltSize]
		nonce := data[HeaderV2Size+SaltSize : HeaderV2Size+SaltSize+NonceSize]
		ciphertext := data[HeaderV2Size+SaltSize+NonceSize:]

		key := DeriveKey(passphrase, salt)
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("create cipher: %w", err)
		}

		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("create gcm: %w", err)
		}

		// Authenticate and decrypt with AAD = data[:9]
		decrypted, err := gcm.Open(nil, nonce, ciphertext, aad)
		if err != nil {
			return nil, ErrDecryptionFailed
		}

		switch flag {
		case CompressionNone:
			if uint32(len(decrypted)) != origSz {
				return nil, fmt.Errorf("%w: chunk size mismatch got %d want %d", ErrDecryptionFailed, len(decrypted), origSz)
			}
			return decrypted, nil

		case CompressionZstd:
			plaintext, err := DecompressData(decrypted, origSz)
			if err != nil {
				return nil, fmt.Errorf("decompress chunk: %w", err)
			}
			return plaintext, nil

		default:
			return nil, fmt.Errorf("unsupported compression type: %d", flag)
		}

	} else if bytes.Equal(data[:4], MagicHeader) {
		// Legacy TGC1 Container
		if len(data) < MinContainerV1Size {
			return nil, ErrDecryptionFailed
		}

		salt := data[HeaderV1Size : HeaderV1Size+SaltSize]
		nonce := data[HeaderV1Size+SaltSize : HeaderV1Size+SaltSize+NonceSize]
		ciphertext := data[HeaderV1Size+SaltSize+NonceSize:]

		key := DeriveKey(passphrase, salt)
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("create cipher: %w", err)
		}

		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("create gcm: %w", err)
		}

		plaintext, err := gcm.Open(nil, nonce, ciphertext, MagicHeader)
		if err != nil {
			return nil, ErrDecryptionFailed
		}

		return plaintext, nil
	}

	return nil, ErrInvalidHeader
}

// HashData calculates SHA-256 hex string of data.
func HashData(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h[:])
}

// HashStream calculates SHA-256 of an io.Reader.
func HashStream(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), n, nil
}
