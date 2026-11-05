package send

import (
	"bytes"
	"compress/gzip"
	"fmt"
)

// Gzip compresses a body for Send with EncodingGzip.
func Gzip(body []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(body); err != nil {
		return nil, fmt.Errorf("gzip body: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("gzip body: %w", err)
	}
	return buf.Bytes(), nil
}
