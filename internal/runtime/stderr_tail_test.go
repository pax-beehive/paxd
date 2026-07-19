package runtime

import (
	"strings"
	"testing"
)

func TestStderrTailKeepsLastBytes(t *testing.T) {
	tail := newStderrTail(16)
	tail.Consume(strings.NewReader("first line\nsecond line\nthird\n"), "[test]")
	got := tail.Tail(0)
	if len(got) > 16 {
		t.Fatalf("tail length = %d, want <= 16", len(got))
	}
	if !strings.Contains(got, "third") {
		t.Fatalf("tail = %q, want to contain most recent line", got)
	}
	if strings.Contains(got, "first line") {
		t.Fatalf("tail = %q, oldest line should be evicted", got)
	}
}

func TestStderrTailTruncatesOversizedLine(t *testing.T) {
	tail := newStderrTail(8)
	tail.Consume(strings.NewReader(strings.Repeat("x", 100)+"END\n"), "[test]")
	got := tail.Tail(0)
	if len(got) > 8 {
		t.Fatalf("tail length = %d, want <= 8", len(got))
	}
	if !strings.HasSuffix(strings.TrimRight(got, "\n"), "END") {
		t.Fatalf("tail = %q, want suffix of oversized line", got)
	}
}

func TestStderrTailMaxParameter(t *testing.T) {
	tail := newStderrTail(64)
	tail.Consume(strings.NewReader("abcdefghij\nklmnopqrst\n"), "[test]")
	got := tail.Tail(5)
	if len(got) > 5 {
		t.Fatalf("Tail(5) length = %d, want <= 5", len(got))
	}
}

func TestStderrTailEmptyReader(t *testing.T) {
	tail := newStderrTail(64)
	tail.Consume(strings.NewReader(""), "[test]")
	if got := tail.Tail(0); got != "" {
		t.Fatalf("Tail() = %q, want empty", got)
	}
}
