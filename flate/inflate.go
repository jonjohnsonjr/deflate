// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package flate implements the DEFLATE compressed data format, described in
// RFC 1951.  The [compress/gzip] and [compress/zlib] packages implement access
// to DEFLATE-based file formats.
package flate

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"math/bits"
	"os"
	"slices"
	"strconv"
	"sync"
)

const (
	maxCodeLen = 16 // max length of Huffman code
	// The next three numbers come from the RFC section 3.2.7, with the
	// additional proviso in section 3.2.5 which implies that distance codes
	// 30 and 31 should never occur in compressed data.
	maxNumLit  = 286
	maxNumDist = 30
	numCodes   = 19 // number of codes in Huffman meta-code
)

// Initialize the fixedHuffmanDecoder only once upon first use.
var fixedOnce sync.Once
var fixedHuffmanDecoder huffmanDecoder

// A CorruptInputError reports the presence of corrupt input at a given offset.
type CorruptInputError int64

func (e CorruptInputError) Error() string {
	return "flate: corrupt input before offset " + strconv.FormatInt(int64(e), 10)
}

// An InternalError reports an error in the flate code itself.
type InternalError string

func (e InternalError) Error() string { return "flate: internal error: " + string(e) }

// A ReadError reports an error encountered while reading input.
//
// Deprecated: No longer returned.
type ReadError struct {
	Offset int64 // byte offset where error occurred
	Err    error // error returned by underlying Read
}

func (e *ReadError) Error() string {
	return "flate: read error at offset " + strconv.FormatInt(e.Offset, 10) + ": " + e.Err.Error()
}

// A WriteError reports an error encountered while writing output.
//
// Deprecated: No longer returned.
type WriteError struct {
	Offset int64 // byte offset where error occurred
	Err    error // error returned by underlying Write
}

func (e *WriteError) Error() string {
	return "flate: write error at offset " + strconv.FormatInt(e.Offset, 10) + ": " + e.Err.Error()
}

// Resetter resets a ReadCloser returned by [NewReader] or [NewReaderDict]
// to switch to a new underlying [Reader]. This permits reusing a ReadCloser
// instead of allocating a new one.
type Resetter interface {
	// Reset discards any buffered data and resets the Resetter as if it was
	// newly initialized with the given reader.
	Reset(r io.Reader, dict []byte) error
}

// The data structure for decoding Huffman tables is based on that of
// zlib. There is a lookup table of a fixed bit width (huffmanChunkBits),
// For codes smaller than the table width, there are multiple entries
// (each combination of trailing bits has the same value). For codes
// larger than the table width, the table contains a link to an overflow
// table. The width of each entry in the link table is the maximum code
// size minus the chunk width.
//
// Note that you can do a lookup in the table even without all bits
// filled. Since the extra bits are zero, and the DEFLATE Huffman codes
// have the property that shorter codes come before longer ones, the
// bit length estimate in the result is a lower bound on the actual
// number of bits.
//
// See the following:
//	https://github.com/madler/zlib/raw/master/doc/algorithm.txt

// chunk & 15 is number of bits
// chunk >> 4 is value, including table link

const (
	huffmanChunkBits  = 9
	huffmanNumChunks  = 1 << huffmanChunkBits
	huffmanCountMask  = 15
	huffmanValueShift = 4
)

type huffmanDecoder struct {
	min      int                      // the minimum code length
	chunks   [huffmanNumChunks]uint32 // chunks as described above
	links    [][]uint32               // overflow links
	linkMask uint32                   // mask the width of the link table

	// For dot file generation - store symbol codes and lengths
	symbolCodes   []uint16 // codes for each symbol (index is symbol value)
	symbolLengths []int    // code lengths for each symbol
}

// Initialize Huffman decoding tables from array of code lengths.
// Following this function, h is guaranteed to be initialized into a complete
// tree (i.e., neither over-subscribed nor under-subscribed). The exception is a
// degenerate case where the tree has only a single symbol with length 1. Empty
// trees are permitted.
func (h *huffmanDecoder) init(lengths []int) bool {
	// Sanity enables additional runtime tests during Huffman
	// table construction. It's intended to be used during
	// development to supplement the currently ad-hoc unit tests.
	const sanity = false

	if h.min != 0 {
		*h = huffmanDecoder{}
	}

	// Count number of codes of each length,
	// compute min and max length.
	var count [maxCodeLen]int
	var min, max int
	for _, n := range lengths {
		if n == 0 {
			continue
		}
		if min == 0 || n < min {
			min = n
		}
		if n > max {
			max = n
		}
		count[n]++
	}

	// Empty tree. The decompressor.huffSym function will fail later if the tree
	// is used. Technically, an empty tree is only valid for the HDIST tree and
	// not the HCLEN and HLIT tree. However, a stream with an empty HCLEN tree
	// is guaranteed to fail since it will attempt to use the tree to decode the
	// codes for the HLIT and HDIST trees. Similarly, an empty HLIT tree is
	// guaranteed to fail later since the compressed data section must be
	// composed of at least one symbol (the end-of-block marker).
	if max == 0 {
		return true
	}

	code := 0
	var nextcode [maxCodeLen]int
	for i := min; i <= max; i++ {
		code <<= 1
		nextcode[i] = code
		code += count[i]
	}

	// Check that the coding is complete (i.e., that we've
	// assigned all 2-to-the-max possible bit sequences).
	// Exception: To be compatible with zlib, we also need to
	// accept degenerate single-code codings. See also
	// TestDegenerateHuffmanCoding.
	if code != 1<<uint(max) && !(code == 1 && max == 1) {
		return false
	}

	h.min = min
	if max > huffmanChunkBits {
		numLinks := 1 << (uint(max) - huffmanChunkBits)
		h.linkMask = uint32(numLinks - 1)

		// create link tables
		link := nextcode[huffmanChunkBits+1] >> 1
		h.links = make([][]uint32, huffmanNumChunks-link)
		for j := uint(link); j < huffmanNumChunks; j++ {
			reverse := int(bits.Reverse16(uint16(j)))
			reverse >>= uint(16 - huffmanChunkBits)
			off := j - uint(link)
			if sanity && h.chunks[reverse] != 0 {
				panic("impossible: overwriting existing chunk")
			}
			h.chunks[reverse] = uint32(off<<huffmanValueShift | (huffmanChunkBits + 1))
			h.links[off] = make([]uint32, numLinks)
		}
	}

	// Store symbol codes and lengths for dot file generation
	h.symbolCodes = make([]uint16, len(lengths))
	h.symbolLengths = make([]int, len(lengths))

	for i, n := range lengths {
		if n == 0 {
			continue
		}
		code := nextcode[n]
		nextcode[n]++

		// Store for dot file generation
		h.symbolCodes[i] = uint16(code)
		h.symbolLengths[i] = n

		chunk := uint32(i<<huffmanValueShift | n)
		reverse := int(bits.Reverse16(uint16(code)))
		reverse >>= uint(16 - n)
		if n <= huffmanChunkBits {
			for off := reverse; off < len(h.chunks); off += 1 << uint(n) {
				// We should never need to overwrite
				// an existing chunk. Also, 0 is
				// never a valid chunk, because the
				// lower 4 "count" bits should be
				// between 1 and 15.
				if sanity && h.chunks[off] != 0 {
					panic("impossible: overwriting existing chunk")
				}
				h.chunks[off] = chunk
			}
		} else {
			j := reverse & (huffmanNumChunks - 1)
			if sanity && h.chunks[j]&huffmanCountMask != huffmanChunkBits+1 {
				// Longer codes should have been
				// associated with a link table above.
				panic("impossible: not an indirect chunk")
			}
			value := h.chunks[j] >> huffmanValueShift
			linktab := h.links[value]
			reverse >>= huffmanChunkBits
			for off := reverse; off < len(linktab); off += 1 << uint(n-huffmanChunkBits) {
				if sanity && linktab[off] != 0 {
					panic("impossible: overwriting existing chunk")
				}
				linktab[off] = chunk
			}
		}
	}

	if sanity {
		// Above we've sanity checked that we never overwrote
		// an existing entry. Here we additionally check that
		// we filled the tables completely.
		for i, chunk := range h.chunks {
			if chunk == 0 {
				// As an exception, in the degenerate
				// single-code case, we allow odd
				// chunks to be missing.
				if code == 1 && i%2 == 1 {
					continue
				}
				panic("impossible: missing chunk")
			}
		}
		for _, linktab := range h.links {
			for _, chunk := range linktab {
				if chunk == 0 {
					panic("impossible: missing chunk")
				}
			}
		}
	}

	return true
}

// writeDotFileWithType generates a graphviz dot file with appropriate labels
func (h *huffmanDecoder) writeDotFileWithType(file io.Writer, getLabel func(int) string) error {
	if h.min == 0 || h.symbolCodes == nil || h.symbolLengths == nil {
		// Empty tree, create a simple dot file
		return writeEmptyDotFile(file)
	}

	fmt.Fprintf(file, "digraph huffman {\n")
	fmt.Fprintf(file, "  rankdir=TB;\n")
	fmt.Fprintf(file, "  ordering=out;\n")
	fmt.Fprintf(file, "  node [shape=circle];\n")

	// Build the tree from stored codes and lengths
	nodeCounter := 0
	nodes := make(map[string]int)
	createdNodes := make(map[int]bool)
	createdEdges := make(map[string]bool)

	// Helper function to get or create a node
	getNode := func(path string) int {
		if id, exists := nodes[path]; exists {
			return id
		}
		id := nodeCounter
		nodeCounter++
		nodes[path] = id
		return id
	}

	// Create root node
	rootID := getNode("")
	fmt.Fprintf(file, "  %d [label=\"root\"];\n", rootID)
	createdNodes[rootID] = true

	symbolToLength := map[int]int{}

	for symbol, length := range h.symbolLengths {
		if length == 0 {
			continue
		}
		symbolToLength[symbol] = length
	}

	for _, symbol := range slices.SortedFunc(maps.Keys(symbolToLength), func(a, b int) int {
		return cmp.Or(cmp.Compare(symbolToLength[a], symbolToLength[b]), cmp.Compare(a, b))
	}) {
		length := symbolToLength[symbol]

		code := h.symbolCodes[symbol]
		// Build the path from the code
		path := ""
		for i := length - 1; i >= 0; i-- {
			if (code>>uint(i))&1 == 1 {
				path += "1"
			} else {
				path += "0"
			}
		}

		// Create the path in the tree
		if err := createPathInTree(file, path, symbol, getNode, createdNodes, createdEdges, getLabel); err != nil {
			return err
		}
	}

	fmt.Fprintf(file, "}\n")
	return nil
}

// getSymbolLabel returns a human-readable label for a DEFLATE symbol
func getSymbolLabel(symbol int) string {
	if symbol < 256 {
		// Literal bytes
		if symbol == 10 {
			// Printable ASCII
			return fmt.Sprintf("%d '\\\\n'", symbol)
		}
		if symbol == 34 {
			return fmt.Sprintf("%d '\\\"'", symbol)
		} else if symbol >= 32 && symbol <= 126 {
			// Printable ASCII
			return fmt.Sprintf("%d '%c'", symbol, symbol)
		} else {
			// Non-printable
			return fmt.Sprintf("%d", symbol)
		}
	} else if symbol == 256 {
		// End of block marker
		return "256 EOB"
	} else if symbol <= 285 {
		// Length codes
		lengthBase := []int{
			3, 4, 5, 6, 7, 8, 9, 10, 11, 13, 15, 17, 19, 23, 27, 31,
			35, 43, 51, 59, 67, 83, 99, 115, 131, 163, 195, 227, 258,
		}
		lengthExtra := []int{
			0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2,
			3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 0,
		}
		idx := symbol - 257
		if idx < len(lengthBase) {
			base := lengthBase[idx]
			extra := lengthExtra[idx]
			if extra == 0 {
				return fmt.Sprintf("%d len=%d", symbol, base)
			} else {
				max := base + (1 << extra) - 1
				return fmt.Sprintf("%d len=%d-%d", symbol, base, max)
			}
		}
	}
	// Unknown or distance codes (shouldn't appear in h1)
	return fmt.Sprintf("%d", symbol)
}

// getDistanceLabel returns a human-readable label for a DEFLATE distance code
func getDistanceLabel(symbol int) string {
	if symbol <= 29 {
		distanceBase := []int{
			1, 2, 3, 4, 5, 7, 9, 13, 17, 25, 33, 49, 65, 97, 129, 193,
			257, 385, 513, 769, 1025, 1537, 2049, 3073, 4097, 6145, 8193, 12289, 16385, 24577,
		}
		distanceExtra := []int{
			0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6,
			7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13,
		}
		if symbol < len(distanceBase) {
			base := distanceBase[symbol]
			extra := distanceExtra[symbol]
			if extra == 0 {
				return fmt.Sprintf("%d dist=%d", symbol, base)
			} else {
				max := base + (1 << extra) - 1
				return fmt.Sprintf("%d dist=%d-%d", symbol, base, max)
			}
		}
	}
	return fmt.Sprintf("%d", symbol)
}

// createPathInTree creates nodes and edges for a given path to a symbol
func createPathInTree(file io.Writer, path string, symbol int, getNode func(string) int, createdNodes map[int]bool, createdEdges map[string]bool, getLabel func(int) string) error {
	currentPath := ""
	currentNodeID := getNode("")

	for i, bit := range path {
		nextPath := currentPath + string(bit)
		nextNodeID := getNode(nextPath)

		// Create the next node if we haven't already
		if !createdNodes[nextNodeID] {
			if i == len(path)-1 {
				label := getLabel(symbol)
				fmt.Fprintf(file, "  %d [label=\"%s\", shape=box];\n", nextNodeID, label)
			} else {
				fmt.Fprintf(file, "  %d [label=\"\"];\n", nextNodeID)
			}
			createdNodes[nextNodeID] = true
		}

		// Create edge with bit label if we haven't already
		edgeKey := fmt.Sprintf("%d->%d:%s", currentNodeID, nextNodeID, string(bit))
		if !createdEdges[edgeKey] {
			fmt.Fprintf(file, "  %d -> %d [label=\"%s\"];\n", currentNodeID, nextNodeID, string(bit))
			createdEdges[edgeKey] = true
		}

		currentPath = nextPath
		currentNodeID = nextNodeID
	}

	return nil
}

// writeEmptyDotFile creates a dot file for an empty huffman tree
func writeEmptyDotFile(file io.Writer) error {
	fmt.Fprintf(file, "digraph huffman {\n")
	fmt.Fprintf(file, "  rankdir=TB;\n")
	fmt.Fprintf(file, "  node [shape=circle];\n")
	fmt.Fprintf(file, "  root [label=\"empty tree\"];\n")
	fmt.Fprintf(file, "}\n")
	return nil
}

// The actual read interface needed by [NewReader].
// If the passed in [io.Reader] does not also have ReadByte,
// the [NewReader] will introduce its own buffering.
type Reader interface {
	io.Reader
	io.ByteReader
}

// Decompress state.
type decompressor struct {
	// Input/output sources.
	r       Reader
	rBuf    *bufio.Reader // created if provided io.Reader does not implement io.ByteReader
	roffset int64
	woffset int64

	// Input bits, in top of b.
	b  uint32
	nb uint

	final bool

	// Huffman decoders for literal/length, distance.
	h1, h2 huffmanDecoder

	// Length arrays used to define Huffman codes.
	bits     *[maxNumLit + maxNumDist]int
	codebits *[numCodes]int

	// Output history, buffer.
	dict dictDecoder

	// Temporary buffer (avoids repeated allocation).
	buf [4]byte

	// TODO: figure out fallible iterators
	err error

	sidechannel *json.Encoder
	block       Block
}

type code struct {
	Len int `json:"len"`
	Val int `json:"val,omitempty"`
	Rep int `json:"rep,omitempty"`
}

type stuff struct {
	Final int `json:"bfinal"`
	Type  int `json:"btype"`
	Len   int `json:"len,omitempty"`

	HCLEN int `json:"hclen,omitempty"`
	HLIT  int `json:"hlit,omitempty"`
	HDIST int `json:"hdist,omitempty"`

	H0    []int  `json:"h0,omitempty"`
	Codes []code `json:"codes,omitempty"`

	H1 []int `json:"h1,omitempty"`
	H2 []int `json:"h2,omitempty"`
}

// RFC 1951 section 3.2.7.
// Compression with dynamic Huffman codes

var codeOrder = [...]int{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}

func (f *decompressor) readHuffman() error {
	// HLIT[5], HDIST[5], HCLEN[4].
	for f.nb < 5+5+4 {
		if err := f.moreBits(); err != nil {
			return err
		}
	}
	nlit := int(f.b&0x1F) + 257
	if nlit > maxNumLit {
		return CorruptInputError(f.roffset)
	}
	f.b >>= 5
	ndist := int(f.b&0x1F) + 1
	if ndist > maxNumDist {
		return CorruptInputError(f.roffset)
	}
	f.b >>= 5
	nclen := int(f.b&0xF) + 4
	// numCodes is 19, so nclen is always valid.
	f.b >>= 4
	f.nb -= 5 + 5 + 4

	// (HCLEN+4)*3 bits: code lengths in the magic codeOrder order.
	for i := 0; i < nclen; i++ {
		for f.nb < 3 {
			if err := f.moreBits(); err != nil {
				return err
			}
		}
		f.codebits[codeOrder[i]] = int(f.b & 0x7)
		f.b >>= 3
		f.nb -= 3
	}
	for i := nclen; i < len(codeOrder); i++ {
		f.codebits[codeOrder[i]] = 0
	}
	if !f.h1.init(f.codebits[0:]) {
		return CorruptInputError(f.roffset)
	}

	var codes []code

	// HLIT + 257 code lengths, HDIST + 1 code lengths,
	// using the code length Huffman code.
	for i, n := 0, nlit+ndist; i < n; {
		x, err := f.huffSym(&f.h1)
		if err != nil {
			return err
		}
		if x < 16 {
			// Actual length.
			f.bits[i] = x
			i++
			codes = append(codes, code{x, 0, 0})
			continue
		}
		// Repeat previous length or zero.
		var rep int
		var nb uint
		var b int
		switch x {
		default:
			return InternalError("unexpected length code")
		case 16:
			rep = 3
			nb = 2
			if i == 0 {
				return CorruptInputError(f.roffset)
			}
			b = f.bits[i-1]
		case 17:
			rep = 3
			nb = 3
			b = 0
		case 18:
			rep = 11
			nb = 7
			b = 0
		}
		for f.nb < nb {
			if err := f.moreBits(); err != nil {
				return err
			}
		}
		rep += int(f.b & uint32(1<<nb-1))
		f.b >>= nb
		f.nb -= nb
		if i+rep > n {
			return CorruptInputError(f.roffset)
		}
		codes = append(codes, code{b, x, rep})
		for j := 0; j < rep; j++ {
			f.bits[i] = b
			i++
		}
	}

	if !f.h1.init(f.bits[0:nlit]) || !f.h2.init(f.bits[nlit:nlit+ndist]) {
		return CorruptInputError(f.roffset)
	}

	f.block = Block{
		Final: f.finalInt(),
		Type:  2,
		HCLEN: nclen,
		HLIT:  nlit,
		HDIST: ndist,
		H0:    f.codebits[0:],
		Codes: codes,
		H1:    f.bits[0:nlit],
		H2:    f.bits[nlit : nlit+ndist],
	}

	// As an optimization, we can initialize the min bits to read at a time
	// for the HLIT tree to the length of the EOB marker since we know that
	// every block must terminate with one. This preserves the property that
	// we never read any extra bytes after the end of the DEFLATE stream.
	if f.h1.min < f.bits[endBlockMarker] {
		f.h1.min = f.bits[endBlockMarker]
	}

	return nil
}

// Decode a single Huffman block from f.
// hl and hd are the Huffman states for the lit/length values
// and the distance values, respectively. If hd == nil, using the
// fixed distance encoding associated with fixed Huffman blocks.
func (f *decompressor) decodeBlock(w io.Writer, hl, hd *huffmanDecoder) error {
	for {
		v, err := f.huffSym(hl)
		if err != nil {
			return err
		}
		var n uint // number of bits extra
		var length int
		switch {
		case v < 256:
			f.dict.writeByte(byte(v))
			if f.dict.availWrite() == 0 {
				if err := f.flush(w); err != nil {
					return err
				}
			}
			continue
		case v == 256:
			return nil
		// otherwise, reference to older data
		case v < 265:
			length = v - (257 - 3)
			n = 0
		case v < 269:
			length = v*2 - (265*2 - 11)
			n = 1
		case v < 273:
			length = v*4 - (269*4 - 19)
			n = 2
		case v < 277:
			length = v*8 - (273*8 - 35)
			n = 3
		case v < 281:
			length = v*16 - (277*16 - 67)
			n = 4
		case v < 285:
			length = v*32 - (281*32 - 131)
			n = 5
		case v < maxNumLit:
			length = 258
			n = 0
		default:
			return CorruptInputError(f.roffset)
		}
		if n > 0 {
			for f.nb < n {
				if err := f.moreBits(); err != nil {
					return err
				}
			}
			length += int(f.b & uint32(1<<n-1))
			f.b >>= n
			f.nb -= n
		}

		var dist int
		if hd == nil {
			for f.nb < 5 {
				if err := f.moreBits(); err != nil {
					return err
				}
			}
			dist = int(bits.Reverse8(uint8(f.b & 0x1F << 3)))
			f.b >>= 5
			f.nb -= 5
		} else {
			if dist, err = f.huffSym(hd); err != nil {
				return err
			}
		}

		switch {
		case dist < 4:
			dist++
		case dist < maxNumDist:
			nb := uint(dist-2) >> 1
			// have 1 bit in bottom of dist, need nb more.
			extra := (dist & 1) << nb
			for f.nb < nb {
				if err := f.moreBits(); err != nil {
					return err
				}
			}
			extra |= int(f.b & uint32(1<<nb-1))
			f.b >>= nb
			f.nb -= nb
			dist = 1<<(nb+1) + 1 + extra
		default:
			return CorruptInputError(f.roffset)
		}

		// No check on length; encoding can be prescient.
		if dist > f.dict.histSize() {
			return CorruptInputError(f.roffset)
		}

		// Perform a backwards copy according to RFC section 3.2.3.
		copyLen := length

		for copyLen > 0 {
			cnt := f.dict.tryWriteCopy(dist, copyLen)
			if cnt == 0 {
				cnt = f.dict.writeCopy(dist, copyLen)
			}
			copyLen -= cnt

			if f.dict.availWrite() == 0 {
				if err := f.flush(w); err != nil {
					return err
				}
			}
		}
	}
	// panic("unreachable")
}

// Copy a single uncompressed data block from input to output.
func (f *decompressor) writeDataBlock(w io.Writer, n int) error {
	if n == 0 {
		return f.flush(w)
	}

	return f.copyData(w, n)
}

// TODO: yield stuff
// Copy a single uncompressed data block from input to output.
func (f *decompressor) dataBlock() (int, error) {
	// Uncompressed.
	// Discard current half-byte.
	f.nb = 0
	f.b = 0

	// Length then ones-complement of length.
	nr, err := io.ReadFull(f.r, f.buf[0:4])
	f.roffset += int64(nr)
	if err != nil {
		return 0, noEOF(err)
	}
	n := int(f.buf[0]) | int(f.buf[1])<<8
	nn := int(f.buf[2]) | int(f.buf[3])<<8
	if uint16(nn) != uint16(^n) {
		return 0, CorruptInputError(f.roffset)
	}

	f.block = Block{
		Final: f.finalInt(),
		Type:  0,
		Len:   n,
		LEN:   f.buf[0:2],
		NLEN:  f.buf[2:4],
	}

	return n, nil
}

// copyData copies f.copyLen bytes from the underlying reader into f.hist.
// It pauses for reads when f.hist is full.
func (f *decompressor) copyData(w io.Writer, copyLen int) error {
	for copyLen > 0 {
		buf := f.dict.writeSlice()
		if len(buf) > copyLen {
			buf = buf[:copyLen]
		}

		cnt, err := io.ReadFull(f.r, buf)
		f.roffset += int64(cnt)
		copyLen -= cnt
		f.dict.writeMark(cnt)
		if err != nil {
			return noEOF(err)
		}

		if f.dict.availWrite() == 0 {
			if err := f.flush(w); err != nil {
				return err
			}
		}
	}

	return nil
}

// noEOF returns err, unless err == io.EOF, in which case it returns io.ErrUnexpectedEOF.
func noEOF(e error) error {
	if e == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return e
}

func (f *decompressor) moreBits() error {
	c, err := f.r.ReadByte()
	if err != nil {
		return noEOF(err)
	}
	f.roffset++
	f.b |= uint32(c) << f.nb
	f.nb += 8
	return nil
}

// Read the next Huffman-encoded symbol from f according to h.
func (f *decompressor) huffSym(h *huffmanDecoder) (int, error) {
	// Since a huffmanDecoder can be empty or be composed of a degenerate tree
	// with single element, huffSym must error on these two edge cases. In both
	// cases, the chunks slice will be 0 for the invalid sequence, leading it
	// satisfy the n == 0 check below.
	n := uint(h.min)
	// Optimization. Compiler isn't smart enough to keep f.b,f.nb in registers,
	// but is smart enough to keep local variables in registers, so use nb and b,
	// inline call to moreBits and reassign b,nb back to f on return.
	nb, b := f.nb, f.b
	for {
		for nb < n {
			c, err := f.r.ReadByte()
			if err != nil {
				f.b = b
				f.nb = nb
				return 0, noEOF(err)
			}
			f.roffset++
			b |= uint32(c) << (nb & 31)
			nb += 8
		}
		chunk := h.chunks[b&(huffmanNumChunks-1)]
		n = uint(chunk & huffmanCountMask)
		if n > huffmanChunkBits {
			chunk = h.links[chunk>>huffmanValueShift][(b>>huffmanChunkBits)&h.linkMask]
			n = uint(chunk & huffmanCountMask)
		}
		if n <= nb {
			if n == 0 {
				f.b = b
				f.nb = nb
				return 0, CorruptInputError(f.roffset)
			}
			f.b = b >> (n & 31)
			f.nb = nb - n
			return int(chunk >> huffmanValueShift), nil
		}
	}
}

func (f *decompressor) makeReader(r io.Reader) {
	if rr, ok := r.(Reader); ok {
		f.rBuf = nil
		f.r = rr
		return
	}
	// Reuse rBuf if possible. Invariant: rBuf is always created (and owned) by decompressor.
	if f.rBuf != nil {
		f.rBuf.Reset(r)
	} else {
		// bufio.NewReader will not return r, as r does not implement flate.Reader, so it is not bufio.Reader.
		f.rBuf = bufio.NewReader(r)
	}
	f.r = f.rBuf
}

func fixedHuffmanDecoderInit() {
	fixedOnce.Do(func() {
		// These come from the RFC section 3.2.6.
		var bits [288]int
		for i := 0; i < 144; i++ {
			bits[i] = 8
		}
		for i := 144; i < 256; i++ {
			bits[i] = 9
		}
		for i := 256; i < 280; i++ {
			bits[i] = 7
		}
		for i := 280; i < 288; i++ {
			bits[i] = 8
		}
		fixedHuffmanDecoder.init(bits[:])
	})
}

// NewReader returns a new ReadCloser that can be used
// to read the uncompressed version of r.
// If r does not also implement [io.ByteReader],
// the decompressor may read more data than necessary from r.
// The reader returns [io.EOF] after the final block in the DEFLATE stream has
// been encountered. Any trailing data after the final block is ignored.
//
// The [io.ReadCloser] returned by NewReader also implements [Resetter].
func NewReader(r io.Reader) io.ReadCloser {
	fixedHuffmanDecoderInit()

	var f decompressor
	f.dict.init(maxMatchOffset, nil)
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(f.decompress(r, pw)) }()
	return pr
}

// NewReaderDict is like [NewReader] but initializes the reader
// with a preset dictionary. The returned reader behaves as if
// the uncompressed data stream started with the given dictionary,
// which has already been read. NewReaderDict is typically used
// to read data compressed by [NewWriterDict].
//
// The ReadCloser returned by NewReaderDict also implements [Resetter].
func NewReaderDict(r io.Reader, dict []byte) io.ReadCloser {
	fixedHuffmanDecoderInit()

	var f decompressor
	f.dict.init(maxMatchOffset, dict)
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(f.decompress(r, pw)) }()
	return pr
}

func (f *decompressor) decompress(r io.Reader, w io.Writer) error {
	f.makeReader(r)
	f.bits = new([maxNumLit + maxNumDist]int)
	f.codebits = new([numCodes]int)

	file, err := os.Create("blocks.json")
	if err != nil {
		return err
	}

	f.sidechannel = json.NewEncoder(file)

	f.woffset = 0

	return errors.Join(f.inflate(w), f.flush(w), file.Close())
}

func (f *decompressor) decompress2(r io.Reader, w io.Writer) error {
	f.makeReader(r)
	f.bits = new([maxNumLit + maxNumDist]int)
	f.codebits = new([numCodes]int)

	file, err := os.Create("blocks.json")
	if err != nil {
		return err
	}

	f.sidechannel = json.NewEncoder(file)

	f.woffset = 0

	return errors.Join(f.inflate(w), f.flush(w), file.Close())
}

func (f *decompressor) finalInt() int {
	if f.final {
		return 1
	}

	return 0
}

func (f *decompressor) inflate(w io.Writer) (err error) {
	f.final = false
	for err == nil && !f.final {
		for f.nb < 1+2 {
			if err = f.moreBits(); err != nil {
				return
			}
		}
		f.final = f.b&1 == 1
		f.b >>= 1
		typ := f.b & 3
		f.b >>= 2
		f.nb -= 1 + 2
		switch typ {
		case 0:
			var n int
			if n, err = f.dataBlock(); err == nil {
				err = f.writeDataBlock(w, n)
			}
		case 1:
			f.block = Block{
				Final: f.finalInt(),
				Type:  1,
			}
			// compressed, fixed Huffman tables
			err = f.decodeBlock(w, &fixedHuffmanDecoder, nil)
		case 2:
			// compressed, dynamic Huffman tables
			if err = f.readHuffman(); err == nil {
				err = f.decodeBlock(w, &f.h1, &f.h2)
			}
		default:
			// 3 is reserved.
			err = CorruptInputError(f.roffset)
		}
	}

	return
}

func (f *decompressor) Err() error {
	return f.err
}

func (f *decompressor) flush(w io.Writer) error {
	if f.dict.availRead() == 0 {
		return nil
	}
	b := f.dict.readFlush() // Flush what's left in case of error

	n, err := w.Write(b)
	if len(b) != n && err == nil {
		err = io.ErrShortWrite
	}
	if err != nil {
		return &WriteError{Offset: f.woffset, Err: err}
	}
	f.woffset += int64(n)
	return nil
}

type Block struct {
	Final int `json:"bfinal"`
	Type  int `json:"btype"`
	Len   int `json:"len,omitempty"`

	LEN  []byte `json:"LEN,omitempty"`
	NLEN []byte `json:"NLEN,omitempty"`

	HCLEN int `json:"hclen,omitempty"`
	HLIT  int `json:"hlit,omitempty"`
	HDIST int `json:"hdist,omitempty"`

	H0    []int  `json:"h0,omitempty"`
	Codes []code `json:"codes,omitempty"`

	H1 []int `json:"h1,omitempty"`
	H2 []int `json:"h2,omitempty"`

	Error error `json:"error,omitempty"`

	// TODO: Track if this was consumed or not?
	writeTo func(w io.Writer) (int64, error)
	f       *decompressor
}

func (b *Block) WriteTo(w io.Writer) (int64, error) {
	return b.writeTo(w)
}

func (b *Block) Symbols(w io.Writer) iter.Seq[Symbol] {
	f := b.f
	hl := &f.h1
	hd := &f.h2

	return func(yield func(Symbol) bool) {
		for {
			v, err := f.huffSym(hl)
			if err != nil {
				yield(Symbol{
					Error: err,
				})
				return
			}
			var n uint // number of bits extra
			var length int
			switch {
			case v < 256:
				if !yield(Symbol{
					Val: v,
				}) {
					return
				}
				f.dict.writeByte(byte(v))
				if f.dict.availWrite() == 0 {
					if err := f.flush(w); err != nil {
						yield(Symbol{
							Error: err,
						})
						return
					}
				}
				continue
			case v == 256:
				yield(Symbol{
					Val: 256,
				})
				return
			// otherwise, reference to older data
			case v < 265:
				length = v - (257 - 3)
				n = 0
			case v < 269:
				length = v*2 - (265*2 - 11)
				n = 1
			case v < 273:
				length = v*4 - (269*4 - 19)
				n = 2
			case v < 277:
				length = v*8 - (273*8 - 35)
				n = 3
			case v < 281:
				length = v*16 - (277*16 - 67)
				n = 4
			case v < 285:
				length = v*32 - (281*32 - 131)
				n = 5
			case v < maxNumLit:
				length = 258
				n = 0
			default:
				yield(Symbol{
					Error: CorruptInputError(f.roffset),
				})
				return
			}
			if n > 0 {
				for f.nb < n {
					if err := f.moreBits(); err != nil {
						yield(Symbol{
							Error: err,
						})
						return
					}
				}
				length += int(f.b & uint32(1<<n-1))
				f.b >>= n
				f.nb -= n
			}

			var dist int
			if hd == nil {
				for f.nb < 5 {
					if err := f.moreBits(); err != nil {
						yield(Symbol{
							Error: err,
						})
						return
					}
				}
				dist = int(bits.Reverse8(uint8(f.b & 0x1F << 3)))
				f.b >>= 5
				f.nb -= 5
			} else {
				if dist, err = f.huffSym(hd); err != nil {
					yield(Symbol{
						Error: err,
					})
					return
				}
			}

			switch {
			case dist < 4:
				dist++
			case dist < maxNumDist:
				nb := uint(dist-2) >> 1
				// have 1 bit in bottom of dist, need nb more.
				extra := (dist & 1) << nb
				for f.nb < nb {
					if err := f.moreBits(); err != nil {
						yield(Symbol{
							Error: err,
						})
						return
					}
				}
				extra |= int(f.b & uint32(1<<nb-1))
				f.b >>= nb
				f.nb -= nb
				dist = 1<<(nb+1) + 1 + extra
			default:
				yield(Symbol{
					Error: CorruptInputError(f.roffset),
				})
				return
			}

			// No check on length; encoding can be prescient.
			if dist > f.dict.histSize() {
				yield(Symbol{
					Error: CorruptInputError(f.roffset),
				})
				return
			}

			// Perform a backwards copy according to RFC section 3.2.3.
			copyLen := length

			if !yield(Symbol{
				Val:  v,
				Len:  length,
				Dist: dist,
			}) {
				return
			}

			for copyLen > 0 {
				cnt := f.dict.tryWriteCopy(dist, copyLen)
				if cnt == 0 {
					cnt = f.dict.writeCopy(dist, copyLen)
				}
				copyLen -= cnt

				if f.dict.availWrite() == 0 {
					if err := f.flush(w); err != nil {
						yield(Symbol{
							Error: err,
						})
						return
					}
				}
			}
		}
		// panic("unreachable")
	}
}

type Symbol struct {
	Val   int
	Len   int
	Dist  int
	Error error
}

func (f *decompressor) writerTo(fn func(w io.Writer) error) func(io.Writer) (int64, error) {
	return func(w io.Writer) (int64, error) {
		start := f.woffset
		err := errors.Join(fn(w), f.flush(w))
		end := f.woffset

		return end - start, err
	}
}

func NewIter(r io.Reader) iter.Seq[Block] {
	fixedHuffmanDecoderInit()

	var f decompressor
	f.dict.init(maxMatchOffset, nil)
	f.makeReader(r)
	f.bits = new([maxNumLit + maxNumDist]int)
	f.codebits = new([numCodes]int)

	return func(yield func(Block) bool) {
		var err error
		for err == nil && !f.final {
			for f.nb < 1+2 {
				if err = f.moreBits(); err != nil {
					return
				}
			}
			f.final = f.b&1 == 1
			f.b >>= 1
			typ := f.b & 3
			f.b >>= 2
			f.nb -= 1 + 2
			switch typ {
			case 0:
				var n int
				if n, err = f.dataBlock(); err == nil {
					fn := func(w io.Writer) error {
						return f.writeDataBlock(w, n)
					}
					f.block.writeTo = f.writerTo(fn)
					f.block.f = &f
				}
			case 1:
				// compressed, fixed Huffman tables
				f.block = Block{
					Final: f.finalInt(),
					Type:  1,
					f:     &f,
				}

				fn := func(w io.Writer) error {
					return f.decodeBlock(w, &fixedHuffmanDecoder, nil)
				}
				f.block.writeTo = f.writerTo(fn)
			case 2:
				// compressed, dynamic Huffman tables
				if err = f.readHuffman(); err == nil {
					fn := func(w io.Writer) error {
						return f.decodeBlock(w, &f.h1, &f.h2)
					}
					f.block.writeTo = f.writerTo(fn)
					f.block.f = &f
				}
			default:
				// 3 is reserved.
				err = CorruptInputError(f.roffset)
				f.block = Block{}
			}

			f.block.Error = err

			if !yield(f.block) {
				break
			}
		}
	}
}
