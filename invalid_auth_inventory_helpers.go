package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func invalidAuthFileOnDisk(row invalidAuthRow) (bool, int64, error) {
	authFile := fileNameIfJSON(row.AuthFile)
	if authFile == "" {
		return false, 0, fmt.Errorf("record has no safe physical JSON file name")
	}
	authDir := configuredAuthDir()
	if authDir == "" {
		return false, 0, fmt.Errorf("CPA auth directory is unavailable")
	}
	info, err := os.Stat(filepath.Join(authDir, authFile))
	if err == nil {
		return true, info.ModTime().UnixMilli(), nil
	}
	if os.IsNotExist(err) {
		return false, 0, nil
	}
	return false, 0, fmt.Errorf("inspect auth file: %w", err)
}

func normalizeAuthFileMTimeMillis(value int64) int64 {
	if value <= 0 {
		return 0
	}
	switch {
	case value >= 100000000000000000:
		return value / 1000000
	case value >= 100000000000000:
		return value / 1000
	case value >= 100000000000:
		return value
	default:
		return value * 1000
	}
}

func bestInvalidAuthFileMTimeMillis(authFile string, fallback int64) int64 {
	fallback = normalizeAuthFileMTimeMillis(fallback)
	if fileNameIfJSON(authFile) == "" {
		return fallback
	}
	present, current, err := invalidAuthFileOnDisk(invalidAuthRow{AuthFile: authFile})
	if err == nil && present && current > 0 {
		return current
	}
	return fallback
}

func invalidAuthInventoryIdentityOverlaps(row invalidAuthRow, candidate configuredAccount) bool {
	for _, pair := range [][2]string{{row.AuthID, candidate.AuthID}, {row.AuthIndex, candidate.AuthIndex}} {
		if left, right := normalizeAccountAlias(pair[0]), normalizeAccountAlias(pair[1]); left != "" && left == right {
			return true
		}
	}
	candidateFile := normalizeAccountAlias(fileNameIfJSON(candidate.AuthFile))
	if candidateFile == "" {
		return false
	}
	for _, value := range []string{row.AuthFile, row.AuthID, row.AuthIndex, row.Source} {
		if normalizeAccountAlias(fileNameIfJSON(value)) == candidateFile {
			return true
		}
	}
	return false
}
