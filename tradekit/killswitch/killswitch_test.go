package killswitch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKillPersistsAcrossRestart(t *testing.T) {
	f := filepath.Join(t.TempDir(), "KILLED")
	s := New(f)
	if s.Killed() {
		t.Fatal("fresh switch with no marker must not start killed")
	}
	s.Kill("pnl floor")
	s.Kill("second reason ignored")
	if !s.Killed() || s.Reason() != "pnl floor" {
		t.Fatalf("killed=%v reason=%q; want true, first reason", s.Killed(), s.Reason())
	}
	if b, err := os.ReadFile(f); err != nil || string(b) != "pnl floor\n" {
		t.Fatalf("marker = %q, %v", b, err)
	}
	if !New(f).Killed() {
		t.Fatal("a restart must come up killed while the marker exists")
	}
	os.Remove(f)
	if New(f).Killed() {
		t.Fatal("removing the marker must re-enable")
	}
}

func TestInMemorySwitch(t *testing.T) {
	s := New("")
	s.Kill("x")
	if !s.Killed() {
		t.Fatal("in-memory switch must still engage")
	}
}

func TestConsumeMarker(t *testing.T) {
	f := filepath.Join(t.TempDir(), "ADOPT")
	if ok, err := ConsumeMarker(f); ok || err != nil {
		t.Fatalf("absent marker: %v, %v; want false, nil", ok, err)
	}
	os.WriteFile(f, nil, 0o640)
	if ok, err := ConsumeMarker(f); !ok || err != nil {
		t.Fatalf("present marker: %v, %v; want true, nil", ok, err)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatal("marker must be deleted once consumed")
	}
}
