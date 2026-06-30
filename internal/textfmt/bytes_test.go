package textfmt

import "testing"

func TestBytes(t *testing.T) {
	tests := []struct {
		name string
		size uint64
		want string
	}{
		{name: "bytes", size: 512, want: "512 B"},
		{name: "kib", size: 1024, want: "1.0 KiB"},
		{name: "mib", size: 2 * 1024 * 1024, want: "2.0 MiB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Bytes(tt.size); got != tt.want {
				t.Fatalf("Bytes(%d) = %q, want %q", tt.size, got, tt.want)
			}
		})
	}
}

func TestSignedBytes(t *testing.T) {
	if got := SignedBytes(-1); got != "0 B" {
		t.Fatalf("SignedBytes(-1) = %q, want 0 B", got)
	}
	if got := SignedBytes(1024); got != "1.0 KiB" {
		t.Fatalf("SignedBytes(1024) = %q, want 1.0 KiB", got)
	}
}

func TestRate(t *testing.T) {
	if got := Rate(512); got != "512 B/s" {
		t.Fatalf("Rate(512) = %q, want 512 B/s", got)
	}
	if got := Rate(2048); got != "2.0 KiB/s" {
		t.Fatalf("Rate(2048) = %q, want 2.0 KiB/s", got)
	}
}
