package runtimetag

import "testing"

func TestFormat(t *testing.T) {
	tests := []struct {
		name    string
		nodeKey string
		version uint64
		want    string
		wantErr bool
	}{
		{name: "semantic identity", nodeKey: "v1:0123456789ABCDEF1111111111111111", version: 1, want: "0123456789abcdef@v1"},
		{name: "fallback identity", nodeKey: "raw-v1:fedcba98765432102222222222222222", version: 9, want: "fedcba9876543210@v9"},
		{name: "zero version", nodeKey: "v1:0123456789abcdef", wantErr: true},
		{name: "short digest", nodeKey: "v1:0123", version: 1, wantErr: true},
		{name: "non hexadecimal", nodeKey: "v1:0123456789abcdeg", version: 1, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Format(test.nodeKey, test.version)
			if (err != nil) != test.wantErr {
				t.Fatalf("Format() error=%v, wantErr=%v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("Format()=%q, want %q", got, test.want)
			}
		})
	}
}
