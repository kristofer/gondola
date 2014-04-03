// +build none

package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"gnd.la/util/internal/gen/genutil"
	"io/ioutil"
	"strconv"
	"strings"
)

func main() {
	data, err := ioutil.ReadFile("table.txt")
	if err != nil {
		panic(err)
	}
	var buf bytes.Buffer
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "/*") || line == "" {
			continue
		}
		sep := strings.IndexByte(line, ':')
		if sep == -1 {
			panic(line)
		}
		val, err := strconv.ParseInt(line[:sep], 0, 32)
		if err != nil {
			panic(err)
		}
		s, err := strconv.Unquote(line[sep+2:])
		if err != nil {
			panic(err)
		}
		if s == "" {
			continue
		}
		if err := binary.Write(&buf, binary.LittleEndian, uint16(val)); err != nil {
			panic(err)
		}
		if err := binary.Write(&buf, binary.LittleEndian, uint8(len(s))); err != nil {
			panic(err)
		}
		buf.WriteString(s)
	}
	var cbuf bytes.Buffer
	w, err := zlib.NewWriterLevel(&cbuf, zlib.BestCompression)
	if err != nil {
		panic(err)
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	buf.Reset()
	buf.WriteString("package unidecode\n")
	buf.WriteString(genutil.AutogenString())
	fmt.Fprintf(&buf, "var tableData = %q;\n", cbuf.String())
	if err := genutil.WriteAutogen("table.go", buf.Bytes()); err != nil {
		panic(err)
	}
}
