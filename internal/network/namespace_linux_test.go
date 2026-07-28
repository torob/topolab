//go:build linux

package network

import (
	"errors"
	"os"
	"slices"
	"testing"
)

func TestWithNamespaceAlwaysRestores(t *testing.T) {
	target, err := os.CreateTemp(t.TempDir(), "namespace")
	if err != nil {
		t.Fatal(err)
	}
	path := target.Name()
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		fn         func() error
		panicValue any
	}{
		{"success", func() error { return nil }, nil},
		{"error", func() error { return errors.New("callback") }, nil},
		{"panic", func() error { panic("boom") }, "boom"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls []int
			defer func() {
				recovered := recover()
				if recovered != test.panicValue {
					t.Fatalf("panic = %#v, want %#v", recovered, test.panicValue)
				}
				if len(calls) != 2 || calls[0] == calls[1] {
					t.Fatalf("setns calls = %v", calls)
				}
			}()
			_ = withNamespaceFiles(path, test.fn, func(fd int) error { calls = append(calls, fd); return nil })
		})
	}
}

func TestNamespacePIDsAreSorted(t *testing.T) {
	fake := &fakeRunner{run: func(program string, args []string) ([]byte, error) {
		return []byte("30\n2\n11\n"), nil
	}}
	got, err := (OSNamespaceOps{Runner: fake}).PIDs(t.Context(), "ns")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []int{2, 11, 30}) {
		t.Fatalf("PIDs = %v", got)
	}
}
