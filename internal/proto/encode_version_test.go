package proto

import (
	"testing"
	"testing/quick"
)

func TestVersionEncodeDecode(t *testing.T) {
	// test the first 10k versions are both unique and roundtrip
	unique := map[string]bool{}
	for i := uint32(0); i < 10000; i++ {
		val := encodeVersion(i)
		if unique[string(val)] {
			t.Errorf("version %v did not uniquely encode", i)
		}
		decoded, err := decodeVersion(val)
		if err != nil {
			t.Fatalf("decode error: %v", err)
		}
		if decoded != i {
			t.Errorf("roundtrip error: %v(%q) != %v", i, val, decoded)
		}
	}
}

func TestQuickcheckVersionEncodeDecode(t *testing.T) {
	if err := quick.Check(func(v uint32) bool {
		val := encodeVersion(v)
		decoded, err := decodeVersion(val)
		if err != nil {
			t.Fatalf("decode error: %v", err)
			return false
		}
		if decoded != v {
			t.Errorf("roundtrip error: %v(%q) != %v", v, val, decoded)
			return false
		}
		return true
	}, &quick.Config{}); err != nil {
		t.Error(err)
	}
}
