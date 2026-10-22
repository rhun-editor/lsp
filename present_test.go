package main

import (
	"reflect"
	"testing"
)

func TestByteTokens(t *testing.T) {
	// line 1 "a😀b = é;" : "b" is UTF-16 column 3 (the emoji is two units), byte column 5
	text := []byte("fn main() {\na😀b = é;\n}\n")
	data := []uint32{
		0, 3, 4, 0, 0, // main
		1, 3, 1, 1, 2, // b, after the emoji
		0, 4, 1, 1, 0, // é: UTF-16 7, byte 9
		5, 0, 1, 0, 0, // past the text: dropped
	}
	got := byteTokens(text, data)
	want := []uint32{0, 3, 4, 0, 0, 1, 5, 1, 1, 2, 0, 4, 2, 1, 0}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if utf16Bytes([]byte("a😀b"), 3) != 5 || utf16Bytes([]byte("ab"), 9) != 2 {
		t.Fatal("utf16Bytes")
	}
}

func TestLegendNames(t *testing.T) {
	got := clean([]string{"function", "bad name", "", "x.y-z_1"}, 3)
	if !reflect.DeepEqual(got, []string{"function", "unknown", "unknown"}) {
		t.Fatalf("clean %v", got)
	}
	if !validResult("r1") || validResult("a b") || validResult("") {
		t.Fatal("validResult")
	}
	if channel(1) != 255 || channel(0.5) != 128 || channel(-1) != 0 {
		t.Fatal("channel")
	}
}
