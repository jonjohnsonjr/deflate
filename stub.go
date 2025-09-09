package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"syscall/js"

	"github.com/jonjohnsonjr/compress/flate"
	"github.com/jonjohnsonjr/compress/gzip"
)

func bin(in []byte) string {
	bins := []string{}
	for _, b := range in {
		bins = append(bins, fmt.Sprintf("%08b", b))
	}
	return strings.Join(bins, " ")
}

func reflate(input string) string {
	var in bytes.Buffer
	zw := gzip.NewWriter(&in)

	if _, err := io.Copy(zw, strings.NewReader(input)); err != nil {
		panic(err)
	}

	if err := zw.Close(); err != nil {
		panic(err)
	}

	w := &bytes.Buffer{}
	for member := range gzip.NewIter(bytes.NewReader(in.Bytes())) {
		if member.Header == nil {
			t := member.Footer
			fmt.Fprintf(w, "<h2>gzip trailer</h2>\n")
			fmt.Fprintf(w, "<table>\n")
			fmt.Fprintf(w, "<tr><td>CRC32</td><td>%s</td></tr>\n", bin(t.CRC32))
			fmt.Fprintf(w, "<tr><td>ISIZE</td><td>%s</td></tr>\n", bin(t.ISIZE))
			fmt.Fprintf(w, "</table>\n")
			// No header means all we got here is a footer, it's over.
			break
		} else {
			h := member.Header
			fmt.Fprintf(w, "<h2>gzip header</h2>\n")
			fmt.Fprintf(w, "<table>\n")
			fmt.Fprintf(w, "<tr><td>ID1</td><td>%08b</td></tr>\n", h.ID1)
			fmt.Fprintf(w, "<tr><td>ID2</td><td>%08b</td></tr>\n", h.ID2)
			fmt.Fprintf(w, "<tr><td>CM</td><td>%08b</td></tr>\n", h.CM)
			fmt.Fprintf(w, "<tr><td>FLG</td><td>%08b</td></tr>\n", h.FLG)
			fmt.Fprintf(w, "<tr><td>MTIME</td><td>%s</td></tr>\n", bin(h.MTIME))
			fmt.Fprintf(w, "<tr><td>XFL</td><td>%08b</td></tr>\n", h.XFL)
			fmt.Fprintf(w, "<tr><td>OS</td><td>%08b</td></tr>\n", h.OS)
			fmt.Fprintf(w, "</table>\n")
		}

		if err := flate.Serve(w, member.Blocks()); err != nil {
			panic(err)
		}
	}

	fmt.Fprintf(w, "<br>\n")
	fmt.Fprintf(w, "<details><summary>gzipped bytes (%d)</summary>\n", in.Len())
	fmt.Fprintf(w, "<pre>\n")
	for _, byt := range in.Bytes() {
		fmt.Fprintf(w, "%08b\n", byt)
	}
	fmt.Fprintf(w, "</pre>\n")
	fmt.Fprintf(w, "</details>\n")

	return w.String()
}

func main() {
	js.Global().Set("reflate", js.FuncOf(func(this js.Value, args []js.Value) any {
		if len(args) != 1 {
			return "rude, tell me your name"
		}
		name := args[0].String()
		return reflate(name)
	}))

	js.Global().Get("onGoInitialized").Invoke()

	<-make(chan struct{})
}
