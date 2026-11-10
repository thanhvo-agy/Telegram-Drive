package e2e_test

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

// isEmoji checks if a rune falls into standard Unicode emoji ranges.
func isEmoji(r rune) bool {
	if r < 0x2000 {
		return false
	}

	// Unicode Emoji and Pictograph blocks:
	// Miscellaneous Symbols and Pictographs: 1F300-1F5FF
	// Emoticons: 1F600-1F64F
	// Transport and Map Symbols: 1F680-1F6FF
	// Supplemental Symbols and Pictographs: 1F900-1F9FF
	// Symbols and Pictographs Extended-A: 1FA70-1FAFF
	// Miscellaneous Symbols: 2600-26FF
	// Dingbats: 2700-27BF
	if (r >= 0x1F300 && r <= 0x1F5FF) ||
		(r >= 0x1F600 && r <= 0x1F64F) ||
		(r >= 0x1F680 && r <= 0x1F6FF) ||
		(r >= 0x1F900 && r <= 0x1F9FF) ||
		(r >= 0x1FA70 && r <= 0x1FAFF) ||
		(r >= 0x2600 && r <= 0x26FF) ||
		(r >= 0x2700 && r <= 0x27BF) {
		return true
	}

	// Standalone emoji characters
	switch r {
	case 0x203C, 0x2049, 0x2122, 0x2139, 0x2194, 0x2195, 0x2196, 0x2197, 0x2198, 0x2199, 0x21A9, 0x21AA,
		0x231A, 0x231B, 0x2328, 0x23CF, 0x23E9, 0x23EA, 0x23EB, 0x23EC, 0x23ED, 0x23EE, 0x23EF, 0x23F0,
		0x23F1, 0x23F2, 0x23F3, 0x23F8, 0x23F9, 0x23FA, 0x24C2, 0x25AA, 0x25AB, 0x25B6, 0x25C0, 0x25FB,
		0x25FC, 0x25FD, 0x25FE, 0x2934, 0x2935, 0x2B05, 0x2B06, 0x2B07, 0x2B1B, 0x2B1C, 0x2B50, 0x2B55,
		0x3030, 0x303D, 0x3297, 0x3299:
		return true
	}

	return false
}

func scanDirectoryForEmojis(root string, targetExts map[string]bool) ([]string, error) {
	var violations []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == ".git" || base == "bin" || base == ".agents" {
				return filepath.SkipDir
			}
			return nil
		}

		ext := filepath.Ext(path)
		if !targetExts[ext] {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			line := scanner.Text()
			for col, r := range line {
				if isEmoji(r) && !unicode.IsControl(r) {
					violations = append(violations,
						fmt.Sprintf("[EMOJI VIOLATION] %s:%d:%d char: %c (U+%04X) context: %s",
							path, lineNum, col+1, r, r, strings.TrimSpace(line)))
				}
			}
		}
		return scanner.Err()
	})

	return violations, err
}

// TestZeroEmojisPolicy_ProductionCode audits all production Go code, embedded Web UI, and tests.
func TestZeroEmojisPolicy_ProductionCode(t *testing.T) {
	repoRoot := getRepoRoot()
	targetExts := map[string]bool{
		".go":   true,
		".html": true,
		".js":   true,
		".css":  true,
	}

	dirsToScan := []string{
		filepath.Join(repoRoot, "cmd"),
		filepath.Join(repoRoot, "pkg"),
		filepath.Join(repoRoot, "web"),
		filepath.Join(repoRoot, "tests"),
	}

	for _, dir := range dirsToScan {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		violations, err := scanDirectoryForEmojis(dir, targetExts)
		if err != nil {
			t.Fatalf("scan error in %s: %v", dir, err)
		}
		if len(violations) > 0 {
			t.Errorf("Production code violates Zero-Emojis policy in %s:\n%s",
				dir, strings.Join(violations, "\n"))
		}
	}
}

// TestZeroEmojisPolicy_LegacyDashboard audits the legacy dashboard directory and escalates defects.
func TestZeroEmojisPolicy_LegacyDashboard(t *testing.T) {
	repoRoot := getRepoRoot()
	dashDir := filepath.Join(repoRoot, "dashboard")
	if _, err := os.Stat(dashDir); os.IsNotExist(err) {
		t.Skip("dashboard directory does not exist")
		return
	}

	violations, err := scanDirectoryForEmojis(dashDir, map[string]bool{".html": true, ".js": true})
	if err != nil {
		t.Fatalf("scan error in dashboard: %v", err)
	}

	// If violations exist in legacy dashboard, report them as a logged warning/escalation
	if len(violations) > 0 {
		t.Logf("ESCALATION NOTICE: Found %d emoji violations in legacy dashboard directory:\n%s",
			len(violations), strings.Join(violations, "\n"))
		// We do not fail the build for un-embedded legacy files, but flag for clean-up
	}
}
