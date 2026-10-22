// Package artifactfetch holds the built-in rules for user-initiated local file
// fetches from the web console, and the content scanner that runs before any
// fetched byte leaves the node. Agent publish_artifact never uses these rules.
//
// Rules are compiled in on purpose: there is no config file, CLI flag or web
// override, so a compromised console or manager cannot widen what is fetchable.
package artifactfetch

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

const (
	CodeInvalidPath          = "invalid_path"
	CodeOutsideHome          = "outside_home"
	CodeHiddenPathDenied     = "hidden_path_denied"
	CodeCredentialPathDenied = "credential_path_denied"
	CodeOwnerOnlyDenied      = "owner_only_denied"
	CodeNotFound             = "not_found"
	CodeNotRegularFile       = "not_regular_file"
	CodePermissionDenied     = "permission_denied"
	CodeTooLarge             = "too_large"
	CodeSecretDetected       = "secret_detected"

	RuleHomeRoot       = "builtin.home-root"
	RuleHiddenPath     = "builtin.hidden-path"
	RuleCredentialPath = "builtin.credential-path"
	RuleSensitiveFile  = "builtin.sensitive-file"
	RuleOwnerOnly      = "builtin.owner-only"
	RuleSizeLimit      = "builtin.size-limit"
	RuleSecretScan     = "builtin.secret-scan"

	// MaxSizeBytes is the S3 single PUT limit the artifact uploader enforces.
	MaxSizeBytes int64 = 5 * 1024 * 1024 * 1024

	maxPathBytes = 8192
)

// Denial is a policy or file-state refusal. It is a normal outcome, not an
// internal error, and its fields are safe to show to the console user.
type Denial struct {
	Code    string
	Rule    string
	Message string
}

func (d *Denial) Error() string { return d.Code + ": " + d.Message }

// Target is a path that passed the pure path rules.
type Target struct {
	// Path is the cleaned absolute path.
	Path string
	// Rel is Path relative to home, suitable for os.Root.
	Rel      string
	Filename string
}

var hiddenAllowlist = map[string]bool{
	".git": true, ".claude": true, ".codex": true, ".agent": true, ".agents": true,
}

var sensitiveExtensions = map[string]bool{
	".pem": true, ".key": true, ".p12": true, ".pfx": true, ".keystore": true,
	".jks": true, ".kdbx": true, ".ppk": true, ".sqlite": true, ".sqlite3": true,
	".db": true,
}

var sensitivePrefixes = []string{"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"}

var sqliteSidecarBases = []string{".sqlite", ".sqlite3", ".db"}

type pathRule struct {
	rule  string
	code  string
	match func(segments []string) bool
	msg   string
}

// pathRules run in order after the home-root checks; the first match wins.
// Segments are lowercased and relative to home.
var pathRules = []pathRule{
	{
		rule: RuleHiddenPath, code: CodeHiddenPathDenied,
		msg:   "Hidden paths cannot be fetched.",
		match: hasHiddenSegment,
	},
	{
		rule: RuleCredentialPath, code: CodeCredentialPathDenied,
		msg:   "This file is a known credential or tool configuration file and cannot be fetched.",
		match: isCredentialPath,
	},
	{
		rule: RuleSensitiveFile, code: CodeCredentialPathDenied,
		msg:   "Key, keystore, password database and SQLite files cannot be fetched.",
		match: isSensitiveFile,
	},
}

// CheckPath applies the pure path rules in their fixed order: invalid_path,
// outside_home, hidden_path_denied, credential-path, sensitive-file. It does
// not touch the filesystem.
func CheckPath(home, rawPath string) (Target, *Denial) {
	if rawPath == "" || len(rawPath) > maxPathBytes ||
		strings.ContainsAny(rawPath, "\x00\r\n") || !filepath.IsAbs(rawPath) {
		return Target{}, &Denial{
			Code: CodeInvalidPath, Rule: RuleHomeRoot,
			Message: "The path must be an absolute file path.",
		}
	}
	cleaned := filepath.Clean(rawPath)
	home = filepath.Clean(home)
	rel, err := filepath.Rel(home, cleaned)
	if err != nil || !filepath.IsLocal(rel) {
		return Target{}, outsideHome()
	}
	segments := strings.Split(strings.ToLower(filepath.ToSlash(rel)), "/")
	for _, rule := range pathRules {
		if rule.match(segments) {
			return Target{}, &Denial{Code: rule.code, Rule: rule.rule, Message: rule.msg}
		}
	}
	return Target{Path: cleaned, Rel: rel, Filename: filepath.Base(cleaned)}, nil
}

func outsideHome() *Denial {
	return &Denial{
		Code: CodeOutsideHome, Rule: RuleHomeRoot,
		Message: "Only files inside the home directory can be fetched.",
	}
}

func hasHiddenSegment(segments []string) bool {
	for _, segment := range segments {
		if strings.HasPrefix(segment, ".") && !hiddenAllowlist[segment] {
			return true
		}
	}
	return false
}

func isCredentialPath(segments []string) bool {
	n := len(segments)
	if n < 2 {
		return false
	}
	parent, base := segments[n-2], segments[n-1]
	switch parent {
	case ".git":
		return base == "config"
	case ".codex":
		return base == "auth.json" || base == "config.toml"
	case ".claude":
		if base == ".credentials.json" {
			return true
		}
		matched, _ := path.Match("settings*.json", base)
		return matched
	}
	return false
}

func isSensitiveFile(segments []string) bool {
	base := segments[len(segments)-1]
	if sensitiveExtensions[path.Ext(base)] {
		return true
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if stem, ok := strings.CutSuffix(base, suffix); ok {
			for _, ext := range sqliteSidecarBases {
				if strings.HasSuffix(stem, ext) {
					return true
				}
			}
		}
	}
	for _, prefix := range sensitivePrefixes {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	return false
}

func ownerOnlyMessage(cleanedPath string) string {
	return fmt.Sprintf(
		"This file is readable only by its owner. Run `chmod 644 %s` on the node to make it fetchable.",
		shellQuote(cleanedPath),
	)
}

func shellQuote(value string) string {
	safe := true
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("/._-+@%:,=~", r)) {
			safe = false
			break
		}
	}
	if safe {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
