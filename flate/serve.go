package flate

import (
	"bytes"
	"fmt"
	"io"
	"iter"
	"os"
	"strconv"
	"strings"
)

func Reverse(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < len(r)/2; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

type item struct {
	bits    string
	comment string
}

type bs struct {
	items []item
	w     *bytes.Buffer
}

func (b *bs) flip(comment, format string, a ...any) {
	s := Reverse(fmt.Sprintf(format, a...))
	b.w.WriteString(s)
	b.items = append(b.items, item{s, comment})
}

func (b *bs) printf(comment, format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	b.w.WriteString(s)
	b.items = append(b.items, item{s, comment})
}

func bin(in []byte) string {
	bins := []string{}
	for _, b := range in {
		bins = append(bins, fmt.Sprintf("%08b", b))
	}
	return strings.Join(bins, " ")
}

func Serve(w io.Writer, blocks iter.Seq[Block]) error {
	var dict dictDecoder
	dict.init(maxMatchOffset, nil)

	bs := &bs{
		w: &bytes.Buffer{},
	}

	for b := range blocks {
		fmt.Fprintf(w, "<h2>block header</h2>\n")

		fmt.Fprintf(w, "<table>\n")
		fmt.Fprintf(w, "<tr><td>BFINAL</td><td>%b</td></tr>\n", b.Final)
		fmt.Fprintf(w, "<tr><td>BTYPE</td><td>%02b</td></tr>\n", b.Type)
		fmt.Fprintf(w, "</table>\n")

		bs.flip("BFINAL", "%b", b.Final)
		bs.flip("BTYPE", "%02b", b.Type)

		fmt.Fprintf(w, "<br>\n")

		if b.Type == 0 {
			fmt.Fprintf(w, "<h2>uncompressed block</h2>\n")
			fmt.Fprintf(w, "<table>\n")
			fmt.Fprintf(w, "<tr><td>LEN</td><td>%s</td></tr>\n", bin(b.LEN))
			fmt.Fprintf(w, "<tr><td>NLEN</td><td>%s</td></tr>\n", bin(b.NLEN))
			fmt.Fprintf(w, "</table>\n")

			bs.printf("padding", "%s", strings.Repeat("0", 8-(bs.w.Len()%8)))

			for _, c := range b.LEN {
				bs.printf("LEN", "%08b", c)
			}
			for _, c := range b.NLEN {
				bs.printf("NLEN", "%08b", c)
			}

			if b.Len != 0 {
				var buf bytes.Buffer
				b.WriteTo(&buf)

				for _, b := range buf.Bytes() {
					bs.printf("uncompressed", "%08b", b)
				}
			}

			continue
		}

		h1Bits := map[int]string{}
		var h1 huffmanDecoder

		if b.Type == 1 {
			h1 = fixedHuffmanDecoder
		} else if b.Type == 2 {
			if !h1.init(b.H1) {
				panic(fmt.Errorf("failed to init"))
			}
		}
		for symbol, length := range h1.symbolLengths {
			if length == 0 {
				continue
			}
			code := h1.symbolCodes[symbol]

			path := ""
			for i := length - 1; i >= 0; i-- {
				if (code>>uint(i))&1 == 1 {
					path += "1"
				} else {
					path += "0"
				}
			}
			h1Bits[symbol] = path
		}

		h2Bits := map[int]string{}
		var h2 huffmanDecoder

		if b.Type == 1 {
			h2 = fixedHuffmanDistances
		} else if b.Type == 2 {
			if !h2.init(b.H2) {
				panic(fmt.Errorf("failed to init"))
			}
		}
		for symbol, length := range h2.symbolLengths {
			if length == 0 {
				continue
			}
			code := h2.symbolCodes[symbol]

			path := ""
			for i := length - 1; i >= 0; i-- {
				if (code>>uint(i))&1 == 1 {
					path += "1"
				} else {
					path += "0"
				}
			}
			h2Bits[symbol] = path
		}

		if b.Type == 2 {
			fmt.Fprintf(w, "<h2>code lengths</h2>\n")

			fmt.Fprintf(w, "<table>\n")
			fmt.Fprintf(w, "<tr><td>HLIT</td><td>%d - 257 = %d</td><td>%05b</td></tr>\n", b.HLIT, b.HLIT-257, b.HLIT-257)
			fmt.Fprintf(w, "<tr><td>HDIST</td><td>%d - 1 = %d</td><td>%05b</td></tr>\n", b.HDIST, b.HDIST-1, b.HDIST-1)
			fmt.Fprintf(w, "<tr><td>HCLEN</td><td>%d - 4 = %d</td><td>%04b</td></tr>\n", b.HCLEN, b.HCLEN-4, b.HCLEN-4)
			fmt.Fprintf(w, "</table>\n")

			bs.flip("HLIT", "%05b", b.HLIT-257)
			bs.flip("HDIST", "%05b", b.HDIST-1)
			bs.flip("HCLEN", "%04b", b.HCLEN-4)

			fmt.Fprintf(w, "<br>\n")

			fmt.Fprintf(w, "<p>(HCLEN + 4) x 3 = %d bits: code lengths for the code length alphabet</p>\n", b.HCLEN*3)

			fmt.Fprintf(w, "<table>\n")

			fmt.Fprintf(w, "<tr>\n")
			for i := range codeOrder {
				fmt.Fprintf(w, "<td>%d</td>", codeOrder[i])
			}
			fmt.Fprintf(w, "\n</tr>\n")

			fmt.Fprintf(w, "<tr>\n")
			for i := range b.HCLEN {
				fmt.Fprintf(w, "<td>%03b</td>", b.H0[codeOrder[i]])

				bs.flip(fmt.Sprintf("HCLEN[%d]", i), "%03b", b.H0[codeOrder[i]])
			}
			fmt.Fprintf(w, "\n</tr>\n")

			fmt.Fprintf(w, "<tr>\n")
			for i := range b.HCLEN {
				fmt.Fprintf(w, "<td>%d</td>", b.H0[codeOrder[i]])
			}
			fmt.Fprintf(w, "\n</tr>\n")

			fmt.Fprintf(w, "</table>\n")
			fmt.Fprintf(w, "<br>\n")
			fmt.Fprintf(w, "<table>\n")

			fmt.Fprintf(w, "<tr>\n")
			for i := range b.H0 {
				fmt.Fprintf(w, "<td>%d</td>", i)
			}
			fmt.Fprintf(w, "\n</tr>\n")

			fmt.Fprintf(w, "<tr>\n")
			for _, v := range b.H0 {
				fmt.Fprintf(w, "<td>%d</td>", v)
			}
			fmt.Fprintf(w, "\n</tr>\n")

			fmt.Fprintf(w, "</table>\n")
			fmt.Fprintf(w, "<br>\n")

			fmt.Fprintf(w, "<h2>code length tree</h2>\n")

			fmt.Fprintf(w, "<div>\n")
			var h0 huffmanDecoder
			if !h0.init(b.H0) {
				fmt.Fprintf(w, "init failed")
				return fmt.Errorf("init failed")
			}
			if err := hdot(w, h0, strconv.Itoa); err != nil {
				fmt.Fprintf(w, "error: %v", err)
				return err
			}
			fmt.Fprintf(w, "</div>\n")
			fmt.Fprintf(w, "<br>\n")

			fmt.Fprintf(w, "<h2>code length codes</h2>\n")

			fmt.Fprintf(w, `<pre>The alphabet for code lengths is as follows:

    0 - 15: Represent code lengths of 0 - 15
        16: Copy the previous code length 3 - 6 times.
            The next 2 bits indicate repeat length
                  (0 = 3, ... , 3 = 6)
               Example:  Codes 8, 16 (+2 bits 11),
                         16 (+2 bits 10) will expand to
                         12 code lengths of 8 (1 + 6 + 5)
        17: Repeat a code length of 0 for 3 - 10 times.
            (3 bits of length)
        18: Repeat a code length of 0 for 11 - 138 times
            (7 bits of length)</pre>
	`)

			fmt.Fprint(w, "<div class=\"row\">\n")
			fmt.Fprint(w, "<div class=\"column\">\n")

			var h huffmanDecoder
			if !h.init(b.H0) {
				panic(fmt.Errorf("failed to init"))
			}

			symbolToBits := map[int]string{}
			for symbol, length := range h.symbolLengths {
				if length == 0 {
					continue
				}
				code := h.symbolCodes[symbol]

				path := ""
				for i := length - 1; i >= 0; i-- {
					if (code>>uint(i))&1 == 1 {
						path += "1"
					} else {
						path += "0"
					}
				}
				symbolToBits[symbol] = path
			}

			fmt.Fprintf(w, "<p>HLIT + 257 code lengths for the literal/length alphabet and HDIST + 1 code lengths for the distance alphabet, encoded using code length Huffman code</p>\n")

			fmt.Fprintf(w, "<table>\n")

			fmt.Fprintf(w, "<tr>\n")
			fmt.Fprintf(w, "<th>bits</th><th>symbol</th><th>len</th><th>rep</th>\n")
			fmt.Fprintf(w, "</tr>\n")
			for _, c := range b.Codes {
				symbol := max(c.Len, c.Val)
				fmt.Fprintf(w, "<tr>\n")
				switch {
				case c.Val <= 15:
					fmt.Fprintf(w, "<td>%s</td>", symbolToBits[symbol])
					bs.printf("lit code length", "%s", symbolToBits[symbol])
				case c.Val == 16:
					fmt.Fprintf(w, "<td>%s + %02b</td>", symbolToBits[symbol], c.Rep-3)
					bs.printf("16 (copy previous 3-6x)", "%s", symbolToBits[symbol])
					bs.flip("extra bits", "%02b", c.Rep-3)
				case c.Val == 17:
					fmt.Fprintf(w, "<td>%s + %03b</td>", symbolToBits[symbol], c.Rep-3)
					bs.printf("17 (3-10 zeroes)", "%s", symbolToBits[symbol])
					bs.flip("extra bits", "%03b", c.Rep-3)
				case c.Val == 18:
					fmt.Fprintf(w, "<td>%s + %07b</td>", symbolToBits[symbol], c.Rep-11)
					bs.printf("18 (11-138 zeroes)", "%s", symbolToBits[symbol])
					bs.flip("extra bits", "%07b", c.Rep-11)
				}

				fmt.Fprintf(w, "<td>%d</td>", symbol)
				fmt.Fprintf(w, "<td>%d</td>", c.Len)
				switch c.Val {
				case 16:
					fmt.Fprintf(w, "<td>3 + %d = %d</td>", c.Rep-3, c.Rep)
				case 17:
					fmt.Fprintf(w, "<td>3 + %d = %d</td>", c.Rep-3, c.Rep)
				case 18:
					fmt.Fprintf(w, "<td>11 + %d = %d</td>", c.Rep-11, c.Rep)
				}

				fmt.Fprintf(w, "\n</tr>\n")
			}

			fmt.Fprintf(w, "</table>\n")
			fmt.Fprintf(w, "</div>\n")

			fmt.Fprint(w, "<div class=\"column\">\n")
			fmt.Fprintf(w, "<p>The literal/length alphabet</p>\n")

			fmt.Fprintf(w, "<table>\n")

			fmt.Fprintf(w, "<tr><th>symbol</th><th># bits</th></tr>\n")

			for i, v := range b.H1 {
				if v == 0 {
					continue
				}
				fmt.Fprintf(w, "<tr>\n")
				fmt.Fprintf(w, "<td>%d</td>", i)
				fmt.Fprintf(w, "<td>%d</td>", v)
				fmt.Fprintf(w, "\n</tr>\n")
			}

			fmt.Fprintf(w, "</table>\n")

			fmt.Fprintf(w, "<p>The distance alphabet</p>\n")

			fmt.Fprintf(w, "<table>\n")

			fmt.Fprintf(w, "<tr><th>symbol</th><th># bits</th></tr>\n")

			for i, v := range b.H2 {
				if v == 0 {
					continue
				}
				fmt.Fprintf(w, "<tr>\n")
				fmt.Fprintf(w, "<td>%d</td>", i)
				fmt.Fprintf(w, "<td>%d</td>", v)
				fmt.Fprintf(w, "\n</tr>\n")
			}

			fmt.Fprintf(w, "</table>\n")

			fmt.Fprintf(w, "</div>\n")
			fmt.Fprintf(w, "</div>\n")

		}

		fmt.Fprintf(w, "<div>\n")
		fmt.Fprintf(w, "<h2><a href=\"#lit\">literals/lengths tree</a></h2>\n")

		if b.Type == 1 {
			fmt.Fprintf(w, "<details>\n")
		}

		if err := hdot(w, h1, getSymbolLabel); err != nil {
			fmt.Fprintf(w, "error: %v", err)
			return err
		}

		if b.Type == 1 {
			fmt.Fprintf(w, "</details>\n")
		}

		fmt.Fprintf(w, "</div>\n")
		fmt.Fprintf(w, "<br>\n")

		fmt.Fprintf(w, "<div>\n")
		fmt.Fprintf(w, "<h2><a href=\"#dist\">distances tree</a></h2>\n")

		if b.Type == 1 {
			fmt.Fprintf(w, "<details>\n")
		}

		if err := hdot(w, h2, getDistanceLabel); err != nil {
			fmt.Fprintf(w, "error: %v", err)
			return err
		}
		if b.Type == 1 {
			fmt.Fprintf(w, "</details>\n")
		}

		fmt.Fprintf(w, "</div>\n")

		fmt.Fprintf(w, "<br>\n")
		fmt.Fprintf(w, "<h2>data</h2>\n")

		fmt.Fprintf(w, "<table>\n")

		fmt.Fprintf(w, "<tr><th>offset</th><th>symbol</th><th>bits</th><th>len</th><th>dist</th><th>bits</th><th>string</th></tr>\n")

		offset := 0

		for symbol := range b.Symbols(os.Stdout) {
			if symbol.Len == 0 {
				if symbol.Val < 256 {
					bs.printf("lit", "%s", h1Bits[symbol.Val])
					fmt.Fprintf(w, "<tr><td>%d</td><td>%d</td><td>%s</td><td></td><td></td><td></td><td>%s</td></tr>\n", offset, symbol.Val, h1Bits[symbol.Val], printable(byte(symbol.Val)))
					dict.writeByte(byte(symbol.Val))
					if dict.availWrite() == 0 {
						// Discard result
						_ = dict.readFlush()
					}
					offset++
				} else if symbol.Val == 256 {
					bs.printf("EOB", "%s", h1Bits[symbol.Val])
					fmt.Fprintf(w, "<tr><td></td><td>%d</td><td>%s</td><td></td><td></td><td></td><td>EOB</td></tr>\n", symbol.Val, h1Bits[symbol.Val])
				}
			} else {
				dist, copyLen := symbol.Dist, symbol.Len

				var str []byte

				for copyLen > 0 {
					cnt := dict.tryWriteCopy(dist, copyLen)
					if cnt == 0 {
						cnt = dict.writeCopy(dist, copyLen)
					}

					str = append(str, escape(dict.hist[dict.wrPos-cnt:dict.wrPos])...)

					copyLen -= cnt

					if dict.availWrite() == 0 {
						// Discard result
						_ = dict.readFlush()
					}
				}

				lb := lengthToSymbolBits(symbol.Len)
				db := distanceToSymbolBits(symbol.Dist)

				lbits := h1Bits[symbol.Val]
				bs.printf("len", "%s", lbits)
				if lb.numbits > 0 {
					lbits = fmt.Sprintf("%s + %0*b", lbits, lb.numbits, lb.extra)
					bs.flip("extra bits", "%0*b", lb.numbits, lb.extra)
				}

				dbits := h2Bits[db.symbol]
				bs.printf("dist", "%s", dbits)
				if db.numbits > 0 {
					dbits = fmt.Sprintf("%s + %0*b", dbits, db.numbits, db.extra)
					bs.flip("extra bits", "%0*b", db.numbits, db.extra)
				}
				fmt.Fprintf(w, "<tr><td>%d</td><td>%d</td><td>%s</td><td>%d</td><td>%d</td><td>%s</td><td>%s</td></tr>\n", offset, symbol.Val, lbits, symbol.Len, symbol.Dist, dbits, str)

				offset += symbol.Len
			}
		}
		fmt.Fprintf(w, "</table>\n")
	}

	fmt.Fprintf(w, "<br>\n")
	fmt.Fprintf(w, "<h2>deflate bitstream</h2>\n")
	fmt.Fprintf(w, "<details><summary>Details</summary>\n")

	fmt.Fprint(w, "<div class=\"row\">\n")
	fmt.Fprint(w, "<div class=\"column\">\n")
	fmt.Fprintf(w, "<table id=\"bitstream\">\n")

	for _, item := range bs.items {
		switch item.comment {
		case "BTYPE", "HLIT", "HDIST", "HCLEN", "extra bits", "uncompressed":
			// UGH THIS IS AWFUL.
			fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td></tr>\n", Reverse(item.bits), item.comment)
		default:
			fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td></tr>\n", item.bits, item.comment)
		}
	}
	fmt.Fprintf(w, "</table>\n")

	fmt.Fprintf(w, "</div>\n")

	fmt.Fprint(w, "<div class=\"column\">\n")

	type cell struct {
		index int
		bits  string
	}

	var cells []cell

	row := 0
	for i, item := range bs.items {
		flipped := Reverse(item.bits)
		for len(flipped) > 0 {
			remainder := 8 - row

			if remainder >= len(flipped) {
				cells = append(cells, cell{i, flipped})
				row += len(flipped)
				break
			}

			offset := len(flipped) - remainder

			cells = append(cells, cell{i, flipped[offset:]})
			flipped = flipped[:offset]
			row = 0
		}
	}
	fmt.Fprintf(w, "<pre style=\"line-height: 1.5em;\">\n")

	count := 0
	var todo []cell
	for _, c := range cells {
		todo = append(todo, c)
		count += len(c.bits)

		if count > 8 {
			fmt.Fprintf(w, "OOPS! COUNT IS %d", count)
		}

		if count == 8 {
			count = 0
			for i := len(todo) - 1; i >= 0; i-- {
				if len(c.bits) == 0 {
					// fucked up something above, oops
					continue
				}
				fmt.Fprintf(w, "<span class=\"bs-%d\">%s</span>", todo[i].index, todo[i].bits)
			}

			fmt.Fprintf(w, strings.Repeat("\n", len(todo)))

			todo = todo[:0]
		}
	}

	for i := len(todo) - 1; i >= 0; i-- {
		fmt.Fprintf(w, "%s", todo[i].bits)
	}

	fmt.Fprintf(w, "</pre>\n")

	fmt.Fprintf(w, "</div>\n")

	fmt.Fprintf(w, "</div>\n")
	fmt.Fprintf(w, "</details>\n")

	return nil
}

func hdot(w io.Writer, h huffmanDecoder, getLabel func(int) string) error {
	in := &bytes.Buffer{}

	if err := h.writeDotFileWithType(in, getLabel); err != nil {
		return err
	}

	fmt.Fprintf(w, "<div class=\"dot\" hidden>\n")
	if _, err := io.Copy(w, in); err != nil {
		return err
	}
	fmt.Fprintf(w, "</div>\n")

	fmt.Fprintf(w, "<table>\n")

	fmt.Fprintf(w, "<tr><th>symbol</th><th># bits</th><th>bits</th></tr>\n")

	for symbol, length := range h.symbolLengths {
		if length == 0 {
			continue
		}
		code := h.symbolCodes[symbol]

		path := ""
		for i := length - 1; i >= 0; i-- {
			if (code>>uint(i))&1 == 1 {
				path += "1"
			} else {
				path += "0"
			}
		}

		fmt.Fprintf(w, "<tr>\n")
		fmt.Fprintf(w, "<td>%d</td>", symbol)
		fmt.Fprintf(w, "<td>%d</td>", length)
		fmt.Fprintf(w, "<td>%s</td>", path)
		fmt.Fprintf(w, "\n</tr>\n")
	}

	fmt.Fprintf(w, "</table>\n")

	return nil
}

func boilerplate(w io.Writer) func() {
	fmt.Fprint(w, "<html>\n")
	fmt.Fprint(w, "<head>\n")
	fmt.Fprint(w, "<style>\n")
	fmt.Fprint(w, "body { font-family: monospace; }\n")
	fmt.Fprint(w, "td { padding: 0.5em; }\n")
	fmt.Fprint(w, "table, th, td { border: 1px solid black; border-collapse: collapse; text-align: center; }\n")
	fmt.Fprint(w, `* {
  box-sizing: border-box;
}

.row {
  display: flex;
}

.column {
  flex: 50%;
  padding: 5px;
}
`)

	fmt.Fprint(w, "</style>\n")
	fmt.Fprint(w, "</head>\n")
	fmt.Fprint(w, "<body>\n")
	return func() {
		fmt.Fprint(w, "</body>\n")
		fmt.Fprint(w, "</html>\n")
	}
}

type symbits struct {
	symbol  int
	numbits int
	extra   int
}

func distanceToSymbolBits(dist int) symbits {
	distanceBase := []int{
		1, 2, 3, 4, 5, 7, 9, 13, 17, 25, 33, 49, 65, 97, 129, 193,
		257, 385, 513, 769, 1025, 1537, 2049, 3073, 4097, 6145, 8193, 12289, 16385, 24577,
	}
	distanceExtra := []int{
		0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6,
		7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13,
	}

	for i := len(distanceBase) - 1; i >= 0; i-- {
		if dist >= distanceBase[i] {
			symbol := i
			base := distanceBase[i]

			return symbits{
				symbol:  symbol,
				numbits: distanceExtra[i],
				extra:   dist - base,
			}
		}
	}

	panic("invalid")
}

func lengthToSymbolBits(length int) symbits {
	lengthBase := []int{
		3, 4, 5, 6, 7, 8, 9, 10, 11, 13, 15, 17, 19, 23, 27, 31,
		35, 43, 51, 59, 67, 83, 99, 115, 131, 163, 195, 227, 258,
	}
	lengthExtra := []int{
		0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2,
		3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 0,
	}
	for i := len(lengthBase) - 1; i >= 0; i-- {
		if length >= lengthBase[i] {
			base := lengthBase[i]

			return symbits{
				symbol:  i + 257,
				numbits: lengthExtra[i],
				extra:   length - base,
			}
		}
	}
	panic("invalid")
}

func printable(b byte) string {
	if b == 10 {
		return "\\n"
	}
	if b >= 32 && b <= 126 {
		return string(b)
	}
	return "."
}

func escape(b []byte) []byte {
	cloned := bytes.Clone(b)
	for i, c := range cloned {
		if c >= 32 && c <= 126 {
			continue
		}
		cloned[i] = '.'
	}

	return cloned
}
