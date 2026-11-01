package system

import (
	"errors"
	"strings"
	"testing"
)

func quotaFiles(limit int64) *FakeFiles {
	files := NewFakeFiles()
	files.SetLimit(limit)

	return files
}

func TestFakeFilesRefuseWritesPastTheLimit(t *testing.T) {
	files := quotaFiles(10)

	if err := files.Write("a.txt", strings.NewReader("123456"), false); err != nil {
		t.Fatal(err)
	}
	if err := files.Write("b.txt", strings.NewReader("12345"), false); !errors.Is(err, ErrOverQuota) {
		t.Fatalf("got %v", err)
	}
	if _, _, err := files.Open("b.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused write must leave nothing behind: %v", err)
	}
	if err := files.Write("c.txt", strings.NewReader("1234"), false); err != nil {
		t.Fatalf("exactly filling the limit is allowed: %v", err)
	}
}

func TestFakeFilesCountPartialUploadsAndTrash(t *testing.T) {
	files := quotaFiles(10)
	info, err := files.CreateUpload("big.bin", 8, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := files.AppendUpload(info.ID, 0, 6, strings.NewReader("123456")); err != nil {
		t.Fatal(err)
	}

	if err := files.Write("a.txt", strings.NewReader("12345"), false); !errors.Is(err, ErrOverQuota) {
		t.Fatalf("a partial upload takes space: %v", err)
	}
	if _, err := files.AppendUpload(info.ID, 6, 2, strings.NewReader("78")); err != nil {
		t.Fatal(err)
	}
	if err := files.FinishUpload(info.ID); err != nil {
		t.Fatal(err)
	}

	item, err := files.Trash("big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Write("a.txt", strings.NewReader("123"), false); !errors.Is(err, ErrOverQuota) {
		t.Fatalf("trash still counts: %v", err)
	}
	if err := files.PurgeTrash(item.ID); err != nil {
		t.Fatal(err)
	}
	if err := files.Write("a.txt", strings.NewReader("123"), false); err != nil {
		t.Fatalf("emptying the trash frees the space: %v", err)
	}
}

func TestFakeFilesChunkPastTheLimitIsOverQuota(t *testing.T) {
	files := quotaFiles(5)
	info, err := files.CreateUpload("big.bin", 8, false)
	if err != nil {
		t.Fatal(err)
	}

	_, err = files.AppendUpload(info.ID, 0, 8, strings.NewReader("12345678"))
	if !errors.Is(err, ErrOverQuota) || errors.Is(err, ErrUploadShort) {
		t.Fatalf("got %v", err)
	}
}

func TestFakeFilesLoweringTheLimitKeepsEveryFile(t *testing.T) {
	files := quotaFiles(100)
	if err := files.Write("a.txt", strings.NewReader("0123456789"), false); err != nil {
		t.Fatal(err)
	}

	files.SetLimit(4)

	if err := files.Write("b.txt", strings.NewReader("x"), false); !errors.Is(err, ErrOverQuota) {
		t.Fatalf("got %v", err)
	}
	file, info, err := files.Open("a.txt")
	if err != nil || info.Size != 10 {
		t.Fatalf("existing data must stay: %v %v", info, err)
	}
	_ = file.Close()
	if err := files.Remove("a.txt"); err != nil {
		t.Fatalf("deleting is always allowed: %v", err)
	}
	if err := files.Write("b.txt", strings.NewReader("1234"), false); err != nil {
		t.Fatalf("deleting frees space: %v", err)
	}
}

func TestFakeFilesWithoutALimitAreUnlimited(t *testing.T) {
	files := NewFakeFiles()

	if err := files.Write("a.txt", strings.NewReader(strings.Repeat("x", 1<<16)), false); err != nil {
		t.Fatal(err)
	}
}
