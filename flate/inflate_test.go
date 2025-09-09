// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package flate

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReaderTruncated(t *testing.T) {
	vectors := []struct{ input, output string }{
		{"\x00", ""},
		{"\x00\f", ""},
		{"\x00\f\x00", ""},
		{"\x00\f\x00\xf3\xff", ""},
		{"\x00\f\x00\xf3\xffhello", "hello"},
		{"\x00\f\x00\xf3\xffhello, world", "hello, world"},
		{"\x02", ""},
		{"\xf2H\xcd", "He"},
		{"\xf2H͙0a\u0084\t", "Hel\x90\x90\x90\x90\x90"},
		{"\xf2H͙0a\u0084\t\x00", "Hel\x90\x90\x90\x90\x90"},
	}

	for i, v := range vectors {
		r := strings.NewReader(v.input)
		zr := NewReader(r)
		b, err := io.ReadAll(zr)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("test %d, error mismatch: got %v, want io.ErrUnexpectedEOF", i, err)
		}
		if string(b) != v.output {
			t.Errorf("test %d, output mismatch: got %q, want %q", i, b, v.output)
		}
	}
}
