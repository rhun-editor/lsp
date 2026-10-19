package main

import "testing"

func TestGlobMatching(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{"**/*.go", "main.go", true}, {"**/*.go", "/work/深い/main.go", true},
		{"/work/*/file?.{go,rs}", "/work/a/file1.rs", true},
		{"/work/*/file?.{go,rs}", "/work/a/b/file1.rs", false},
		{"**/file[0-9].go", "/file2.go", true}, {"**/file[!0-9].go", "/filex.go", true},
		{"**/file[!0-9].go", "/file2.go", false}, {"**/{a,{b,c}}.go", "/c.go", true},
		{`**/literal\*.go`, "/literal*.go", true}, {"*.go", "/x/main.go", false},
	} {
		r, err := compileGlob(tc.pattern, false)
		if err != nil {
			t.Fatal(tc.pattern, err)
		}
		if got := r.MatchString(tc.path); got != tc.want {
			t.Fatalf("%q %q: %v", tc.pattern, tc.path, got)
		}
	}
	r, err := compileGlob("C:/**/MAIN.GO", true)
	if err != nil || !r.MatchString("c:/work/main.go") {
		t.Fatal(r, err)
	}
}
func TestGlobRejectsInvalidPatterns(t *testing.T) {
	for _, pattern := range []string{"[", "[]", "[z-a]", "{a,b", "a}", "x\\", "[/]"} {
		if _, err := compileGlob(pattern, false); err == nil {
			t.Fatal("accepted", pattern)
		}
	}
}
