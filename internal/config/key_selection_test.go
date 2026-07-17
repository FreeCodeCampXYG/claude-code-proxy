package config

import "testing"

func TestSelectedOpenAIAPIKey(t *testing.T) {
	tests := []struct {
		name       string
		single     string
		multiple   string
		index      string
		wantKey    string
		wantIndex  int
		wantLabel  string
		wantErr    bool
	}{
		{name: "legacy single key", single: "first", wantKey: "first", wantIndex: 1, wantLabel: "key-1"},
		{name: "first multiple key", multiple: "first, second", wantKey: "first", wantIndex: 1, wantLabel: "key-1"},
		{name: "selected multiple key", multiple: "first, second", index: "2", wantKey: "second", wantIndex: 2, wantLabel: "key-2"},
		{name: "empty multiple entry", multiple: "first,,second", wantErr: true},
		{name: "zero index", multiple: "first,second", index: "0", wantErr: true},
		{name: "non numeric index", multiple: "first,second", index: "two", wantErr: true},
		{name: "out of range index", multiple: "first,second", index: "3", wantErr: true},
		{name: "conflicting key settings", single: "first", multiple: "second", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, index, label, err := selectedOpenAIAPIKey(tt.single, tt.multiple, tt.index)
			if (err != nil) != tt.wantErr {
				t.Fatalf("selectedOpenAIAPIKey() error = %v, wantErr %t", err, tt.wantErr)
			}
			if !tt.wantErr && (key != tt.wantKey || index != tt.wantIndex || label != tt.wantLabel) {
				t.Fatalf("selectedOpenAIAPIKey() = %q, %d, %q", key, index, label)
			}
		})
	}
}
