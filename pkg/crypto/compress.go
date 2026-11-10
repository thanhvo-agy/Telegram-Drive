package crypto

import (
	"errors"
	"fmt"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// CompressionType represents the compression algorithm used in TGC2 chunks.
type CompressionType uint8

const (
	// CompressionNone indicates raw uncompressed data inside AES-256-GCM.
	CompressionNone CompressionType = 0x00
	// CompressionZstd indicates Zstandard compression before AES-256-GCM.
	CompressionZstd CompressionType = 0x01
)

var (
	encoderPool = sync.Pool{
		New: func() interface{} {
			enc, err := zstd.NewWriter(nil,
				zstd.WithEncoderLevel(zstd.SpeedDefault),
				zstd.WithEncoderConcurrency(1),
				zstd.WithZeroFrames(true),
			)
			if err != nil {
				return nil
			}
			return enc
		},
	}

	decoderPool = sync.Pool{
		New: func() interface{} {
			dec, err := zstd.NewReader(nil,
				zstd.WithDecoderConcurrency(1),
			)
			if err != nil {
				return nil
			}
			return dec
		},
	}
)

// CompressData compresses input data using Zstandard synchronously without background goroutines.
func CompressData(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return []byte{}, nil
	}

	encObj := encoderPool.Get()
	if encObj == nil {
		return nil, errors.New("failed to initialize zstd encoder")
	}
	enc := encObj.(*zstd.Encoder)
	defer encoderPool.Put(enc)

	compressed := enc.EncodeAll(data, make([]byte, 0, len(data)))
	return compressed, nil
}

// DecompressData decompresses Zstandard data into a pre-allocated buffer sized to origSz.
func DecompressData(data []byte, origSz uint32) ([]byte, error) {
	if origSz == 0 {
		return []byte{}, nil
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty data with non-zero original size", ErrDecompressFailed)
	}

	decObj := decoderPool.Get()
	if decObj == nil {
		return nil, errors.New("failed to initialize zstd decoder")
	}
	dec := decObj.(*zstd.Decoder)
	defer decoderPool.Put(dec)

	dst := make([]byte, 0, origSz)
	result, err := dec.DecodeAll(data, dst)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecompressFailed, err)
	}
	if uint32(len(result)) != origSz {
		return nil, fmt.Errorf("%w: decompressed size mismatch: got %d, want %d", ErrDecompressFailed, len(result), origSz)
	}
	return result, nil
}
