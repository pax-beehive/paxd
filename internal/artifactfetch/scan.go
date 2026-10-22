package artifactfetch

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
)

const (
	defaultWindowBytes  = 1 << 20
	defaultOverlapBytes = 4 << 10
	binarySniffBytes    = 8 << 10
)

type secretPattern struct {
	rule string
	re   *regexp.Regexp
}

// secretPatterns are high-signal token formats. No pattern can span a newline,
// so the overlap only has to cover the longest realistic single match.
var secretPatterns = []secretPattern{
	{"private-key", regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----`)},
	{"aws-access-key-id", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"github-token", regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}\b`)},
	{"github-token", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{60,}\b`)},
	{"slack-token", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`)},
	{"anthropic-api-key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{"openai-api-key", regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{32,}`)},
	{"google-api-key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"stripe-secret-key", regexp.MustCompile(`\b[sr]k_live_[0-9A-Za-z]{24,}\b`)},
	// pax-manager issues node keys as "pax_" and user API keys as "paxu_",
	// each followed by 24 random bytes in lowercase hex.
	{"pax-api-key", regexp.MustCompile(`\bpaxu?_[0-9a-f]{48}\b`)},
}

// Finding identifies the first secret-like match. It deliberately carries no
// matched text so it can be logged and reported safely.
type Finding struct {
	Rule string
	Line int
}

func (f Finding) Message() string {
	return fmt.Sprintf("possible secret detected (rule %s, line %d)", f.Rule, f.Line)
}

// Scanner streams content through overlapping windows so memory stays bounded
// regardless of file size. Zero values select production defaults.
type Scanner struct {
	WindowBytes  int
	OverlapBytes int
}

// ScanFile scans the file at path with default settings.
func ScanFile(path string) (*Finding, error) {
	file, err := os.Open(path) // #nosec G304 -- path is a daemon-owned spool snapshot.
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return Scanner{}.Scan(file)
}

// Scan returns the first finding, or nil when the content is clean or binary
// (a NUL byte within the first 8 KiB).
func (s Scanner) Scan(r io.Reader) (*Finding, error) {
	window, overlap := s.WindowBytes, s.OverlapBytes
	if window <= 0 {
		window = defaultWindowBytes
	}
	if overlap <= 0 {
		overlap = defaultOverlapBytes
	}
	if overlap >= window {
		overlap = window / 2
	}
	reader := bufio.NewReaderSize(r, binarySniffBytes)
	head, err := reader.Peek(binarySniffBytes)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return nil, err
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return nil, nil
	}

	buf := make([]byte, 0, window+overlap)
	chunk := make([]byte, window)
	linesBefore := 0 // newlines in all bytes preceding buf[0]
	carried := 0     // bytes at the start of buf already scanned last round
	for {
		n, readErr := io.ReadFull(reader, chunk)
		eof := errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF)
		if readErr != nil && !eof {
			return nil, readErr
		}
		buf = append(buf, chunk[:n]...)
		if n > 0 || carried == 0 {
			if finding := scanWindow(buf, carried, overlap, eof); finding != nil {
				finding.Line += linesBefore
				return finding, nil
			}
		}
		if eof {
			return nil, nil
		}
		if cut := len(buf) - overlap; cut > 0 {
			linesBefore += bytes.Count(buf[:cut], []byte{'\n'})
			buf = append(buf[:0], buf[cut:]...)
		}
		carried = len(buf)
	}
}

// scanWindow searches buf. A match shorter than the overlap that touches the
// end of a non-final window is skipped because the next window re-scans it
// with its real right-hand context; a short match lying entirely inside the
// carried prefix was already judged with its real left-hand context. Longer
// matches are always reported, failing closed.
func scanWindow(buf []byte, carried, overlap int, final bool) *Finding {
	var best *Finding
	bestStart := len(buf) + 1
	for _, pattern := range secretPatterns {
		for _, loc := range pattern.re.FindAllIndex(buf, -1) {
			start, end := loc[0], loc[1]
			short := end-start < overlap
			if short && !final && end == len(buf) {
				continue
			}
			if short && carried > 0 && end < carried {
				continue
			}
			if start < bestStart {
				bestStart = start
				best = &Finding{Rule: pattern.rule, Line: bytes.Count(buf[:start], []byte{'\n'}) + 1}
			}
			break
		}
	}
	return best
}
