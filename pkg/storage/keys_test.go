package storage

import (
	"errors"
	"strings"
	"testing"
)

func TestObjectKeys(t *testing.T) {
	if got := ChunkObjectKey(7, 12, 2); got != "chunks/7/12/2" {
		t.Fatalf("unexpected chunk key: %s", got)
	}
	if got := ChunkAttemptObjectKey(7, 12, 2, "attempt"); got != "chunks/7/12/attempts/2/attempt" || got == ChunkObjectKey(7, 12, 2) {
		t.Fatalf("unsafe chunk attempt key: %s", got)
	}
	if got := DocumentObjectKey(7, 12, "attempt"); got != "merged/7/12/attempt" {
		t.Fatalf("unexpected document key: %s", got)
	}
	if got := DocumentIRObjectKey(7, 12, "v1"); got != "document-ir/7/12/v1/ir.json" {
		t.Fatalf("unexpected versioned IR key: %s", got)
	}
	if err := verifyMD5(strings.NewReader("hello"), "5d41402abc4b2a76b9719d911017c592"); err != nil {
		t.Fatalf("valid MD5 rejected: %v", err)
	}
	if err := verifyMD5(strings.NewReader("other"), "5d41402abc4b2a76b9719d911017c592"); !errors.Is(err, ErrMD5Mismatch) {
		t.Fatal("mismatched MD5 accepted")
	}
}
