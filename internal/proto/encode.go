package proto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	"github.com/pkg/errors"
)

// WriteVersionedJSONBlob writes a JSON blob to the given writer. It expects
// the blob to be read using 'ReadVersionedJSONBlob'.
// A version is included via a v0 compatible hack since v0 did not include the
// version. Specifically, the version is encoded as whitespace prefixing the
// json data.
func WriteVersionedJSONBlob(dst io.Writer, obj interface{}, version uint32) error {
	versionPrefix := encodeVersion(version)

	var jsonBlob bytes.Buffer
	jsonBlob.Write(versionPrefix)
	enc := json.NewEncoder(&jsonBlob)
	if err := enc.Encode(obj); err != nil {
		return err
	}

	var jsonBlobLenBuf bytes.Buffer
	if err := binary.Write(&jsonBlobLenBuf, binary.BigEndian, int32(jsonBlob.Len())); err != nil {
		panic(fmt.Errorf("could not binary encode an int32: %v", err))
	}
	if jsonBlobLenBuf.Len() != 4 {
		panic(fmt.Errorf("int32 should be 4 bytes, not: %+v", jsonBlobLenBuf))
	}

	// Length-prefixed json blob
	if _, err := dst.Write(jsonBlobLenBuf.Bytes()); err != nil {
		return fmt.Errorf("could not write json length: %v", err)
	}
	if _, err := dst.Write(jsonBlob.Bytes()); err != nil {
		return fmt.Errorf("could not write json: %v", err)
	}
	return nil
}

// WriteJSONBlob writes a length-prefixed json blob.
func WriteJSONBlob(dst io.Writer, obj interface{}) error {
	return WriteVersionedJSONBlob(dst, obj, 0)
}

// ReadVersionedJSONBlob reads a JSON blob from the given writer. If the blob
// was written with WriteVersionedJSONBlob, it determines the version and
// returns it.
func ReadVersionedJSONBlob(src io.Reader, obj interface{}) (uint32, error) {
	var jsonLen int32
	if err := binary.Read(src, binary.BigEndian, &jsonLen); err != nil {
		return 0, errors.Wrap(err, "protocol error: could not read length of json")
	}

	// don't decode directly from src, but rathre go through a buffer, because
	// `json.Decode` will attempt to use a buffered reader which can accidentally
	// lose fd's being sent across a socket.
	data := make([]byte, jsonLen)
	if n, err := io.ReadFull(src, data); err != nil || n != int(jsonLen) {
		return 0, errors.Wrapf(err, "unable to read expected meta json length (expected %v, got (%v, %v))", jsonLen, n, err)
	}
	var prefix []byte
	for i := 0; i < len(data); i++ {
		if !isJSONIgnorableWhitespace(data[i]) {
			prefix = data[0:i]
			break
		}
	}
	version, err := decodeVersion(prefix)
	if err != nil {
		return 0, errors.Wrapf(err, "could not determine version from prefix")
	}

	if err := json.Unmarshal(data, obj); err != nil {
		return 0, errors.Wrap(err, "can't decode names from owner process")
	}
	return version, nil
}

// ReadJSONBlob reads a length-prefixed json blob written by WriteJSONBlob.
func ReadJSONBlob(src io.Reader, obj interface{}) error {
	_, err := ReadVersionedJSONBlob(src, obj)
	return err
}
