package main

import (
	"bytes"
	"fmt"
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
	want := []uint64{0, 3, 4, 0, 0, 1, 5, 1, 1, 2, 0, 4, 2, 1, 0}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if utf16Bytes([]byte("a😀b"), 3) != 5 || utf16Bytes([]byte("ab"), 9) != 2 {
		t.Fatal("utf16Bytes")
	}
}

func TestByteTokensLongUnicodeLine(t *testing.T) {
	text := bytes.Repeat([]byte("a😀bé "), 500)
	text = append(text, []byte("\n😀\n")...)
	var data []uint32
	var want []uint64
	for i := 0; i < 500; i++ {
		unitDelta, byteDelta := uint32(2), uint64(3)
		if i == 0 {
			unitDelta, byteDelta = 0, 0
		}
		data = append(data, 0, unitDelta, 1, 0, 0, 0, 1, 2, 1, 2,
			0, 2, 1, 0, 0, 0, 1, 1, 1, 0)
		want = append(want, 0, byteDelta, 1, 0, 0, 0, 1, 4, 1, 2,
			0, 4, 1, 0, 0, 0, 1, 2, 1, 0)
	}
	data = append(data, 1, 0, 2, 1, 0)
	want = append(want, 1, 0, 4, 1, 0)
	if got := byteTokens(text, data); !reflect.DeepEqual(got, want) {
		t.Fatal("long-line token positions changed")
	}
}

func TestByteTokensSurrogateBoundariesAndSkippedTokens(t *testing.T) {
	text := []byte("😀b\nx😀y")
	data := []uint32{0, 0, 1, 0, 0, 0, 1, 1, 1, 0, 0, 1, 0, 0, 0,
		0, 0, 1, 2, 0, 0, 20, 1, 0, 0, 1, 1, 2, 3, 0, 0, 2, 1, 4, 0}
	want := []uint64{0, 0, 4, 0, 0, 0, 4, 1, 2, 0, 1, 1, 4, 3, 0, 0, 4, 1, 4, 0}
	if got := byteTokens(text, data); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
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

func TestInlineText(t *testing.T) {
	for in, want := range map[string]string{
		`"plain"`: "plain",
		`{"kind":"snippet","value":"f(${1:a}, $2) {\n\t$0\n}"}`: "f(a, ) {\n\t\n}",
		`{"kind":"snippet","value":"\\$x \\} ${3}"}`:            "$x } ",
		`{"kind":"other","value":"${1:x}"}`:                     "${1:x}",
	} {
		if got := inlineText([]byte(in)); got != want {
			t.Errorf("inlineText(%s) = %q, want %q", in, got, want)
		}
	}
}

func BenchmarkByteTokensLongLine(b *testing.B) {
	for _, count := range []int{2000, 8000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			text := bytes.Repeat([]byte("name "), count)
			data := make([]uint32, 0, count*5)
			for i := 0; i < count; i++ {
				delta := uint32(5)
				if i == 0 {
					delta = 0
				}
				data = append(data, 0, delta, 4, 0, 0)
			}
			b.ResetTimer()
			for b.Loop() {
				if got := byteTokens(text, data); len(got) != len(data) {
					b.Fatal(len(got))
				}
			}
		})
	}
}
