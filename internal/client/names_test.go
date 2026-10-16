package client

import (
	"strings"
	"testing"

	"github.com/garyships/kudzu/internal/object"
	"github.com/garyships/kudzu/internal/sparse"
)

func TestCheckName(t *testing.T) {
	everywhere := []string{"", ".", "..", "a/b", "a\x00b", ".git", ".GIT", ".Git", ".git.", ".git ", ".git. .", "git~1", "GIT~1", ".kudzu", ".KUDZU", ".kudzu.", "kudzu~1"}
	windowsOnly := []string{`..\..\evil.bat`, `a\b`, "C:evil", "a:b", "a*", "a?", `a"b`, "a<b", "a>b", "a|b", "a\x01b", "tab\t",
		"a.", "a ", "...", "CON", "con", "Con.txt", "nul.tar.gz", "AUX", "prn.c", "COM1", "com9.log", "LPT1", "lpt9", "COM0", "COM¹", "lpt³.txt", "CON .txt", "conin$", "CONOUT$"}
	ok := []string{"a", "README.md", ".gitignore", ".github", ".gitkeep", ".kudzuignore", "git", "x.git", "git~2", "kudzu", ".gitattributes",
		"CONSOLE", "con1", "COM10", "LPT", "COMA", "auxiliary.c", "nul_device", "é", "a b", " a", ".a"}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, n := range everywhere {
			if checkName(goos, n) == nil {
				t.Errorf("%s: %q accepted", goos, n)
			}
		}
		for _, n := range ok {
			if err := checkName(goos, n); err != nil {
				t.Errorf("%s: %q rejected: %v", goos, n, err)
			}
		}
		for _, n := range windowsOnly {
			err := checkName(goos, n)
			if goos == "windows" && err == nil {
				t.Errorf("%s: %q accepted", goos, n)
			}
			if goos != "windows" && err != nil {
				t.Errorf("%s: %q rejected: %v", goos, n, err)
			}
		}
	}
}

func TestCheckDirCaseCollisions(t *testing.T) {
	id := object.Hash(object.KindBlob, []byte("x"))
	entries := []object.Entry{
		{Name: "A", Mode: object.ModeSymlink, ID: id},
		{Name: "README", Mode: object.ModeFile, ID: id},
		{Name: "a", Mode: object.ModeDir, ID: object.EmptyTreeID},
		{Name: "readme", Mode: object.ModeFile, ID: id},
	}
	for _, goos := range []string{"darwin", "windows"} {
		err := checkDir(goos, "sub", entries, nil)
		if err == nil || !strings.Contains(err.Error(), "sub/A") || !strings.Contains(err.Error(), "sub/a") {
			t.Errorf("%s: A and a not refused: %v", goos, err)
		}
		// Only what the checkout creates counts: a directory outside the
		// sparse cone, or a submodule, is never written.
		sp := sparse.Spec{"sub/b"}
		out := []object.Entry{
			{Name: "Lib", Mode: object.ModeDir, ID: object.EmptyTreeID},
			{Name: "b", Mode: object.ModeDir, ID: object.EmptyTreeID},
			{Name: "lib", Mode: object.ModeDir, ID: object.EmptyTreeID},
			{Name: "x", Mode: object.ModeFile, ID: id},
		}
		if err := checkDir(goos, "sub", out, sp); err != nil {
			t.Errorf("%s: directories outside the sparse cone checked: %v", goos, err)
		}
		mod := []object.Entry{{Name: "M", Mode: object.ModeGitlink, ID: id}, {Name: "m", Mode: object.ModeFile, ID: id}}
		if err := checkDir(goos, "", mod, nil); err != nil {
			t.Errorf("%s: submodule checked: %v", goos, err)
		}
	}
	if err := checkDir("linux", "sub", entries, nil); err != nil {
		t.Errorf("linux: %v", err)
	}
	if err := checkDir("linux", "sub", []object.Entry{{Name: ".git", Mode: object.ModeDir, ID: object.EmptyTreeID}}, nil); err == nil || !strings.Contains(err.Error(), "sub/.git") {
		t.Errorf("linux: sub/.git not refused: %v", err)
	}
	// Simple case folding, as strings.EqualFold: the Kelvin sign is a K.
	if foldCase("Kelvin") != foldCase("kELVIN") || foldCase("straße") == foldCase("strasse") {
		t.Error("foldCase disagrees with strings.EqualFold")
	}
}
