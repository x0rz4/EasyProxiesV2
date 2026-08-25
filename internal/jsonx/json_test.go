package jsonx

import (
	"bytes"
	stdjson "encoding/json"
	"io"
	"strings"
	"testing"
)

type benchmarkNode struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	URI       string   `json:"uri"`
	Enabled   bool     `json:"enabled"`
	LatencyMS int      `json:"latency_ms"`
	Tags      []string `json:"tags"`
}

func TestMarshalCanonicalSortsMapKeys(t *testing.T) {
	value := map[string]any{}
	value["z"] = 3
	value["a"] = map[string]int{"y": 2, "b": 1}

	const want = `{"a":{"b":1,"y":2},"z":3}`
	for range 100 {
		got, err := MarshalCanonical(value)
		if err != nil {
			t.Fatalf("MarshalCanonical: %v", err)
		}
		if string(got) != want {
			t.Fatalf("canonical JSON is not stable: got %s, want %s", got, want)
		}
	}
}

func TestStreamingEncoderDecoder(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
		Port int    `json:"port"`
	}

	var buffer bytes.Buffer
	encoder := NewEncoder(&buffer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(payload{Name: "香港", Port: 1080}); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	decoder := NewDecoder(strings.NewReader(buffer.String()))
	decoder.DisallowUnknownFields()
	var got payload
	if err := decoder.Decode(&got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Name != "香港" || got.Port != 1080 {
		t.Fatalf("unexpected round trip: %+v", got)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("expected EOF after one value, got %v", err)
	}
}

func TestDecoderRejectsUnknownFields(t *testing.T) {
	decoder := NewDecoder(strings.NewReader(`{"known":true,"unexpected":1}`))
	decoder.DisallowUnknownFields()
	var value struct {
		Known bool `json:"known"`
	}
	if err := decoder.Decode(&value); err == nil {
		t.Fatal("expected unknown field error")
	}
}

func benchmarkNodes() []benchmarkNode {
	nodes := make([]benchmarkNode, 256)
	for i := range nodes {
		nodes[i] = benchmarkNode{
			ID: int64(i + 1), Name: "proxy-node", URI: "vless://uuid@example.com:443?security=tls&type=ws",
			Enabled: true, LatencyMS: 42 + i%100, Tags: []string{"HK", "streaming"},
		}
	}
	return nodes
}

func BenchmarkMarshalNodes(b *testing.B) {
	nodes := benchmarkNodes()
	b.Run("encoding-json", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := stdjson.Marshal(nodes); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("sonic", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := Marshal(nodes); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkUnmarshalNodes(b *testing.B) {
	data, err := stdjson.Marshal(benchmarkNodes())
	if err != nil {
		b.Fatal(err)
	}
	b.Run("encoding-json", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var nodes []benchmarkNode
			if err := stdjson.Unmarshal(data, &nodes); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("sonic", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var nodes []benchmarkNode
			if err := Unmarshal(data, &nodes); err != nil {
				b.Fatal(err)
			}
		}
	})
}
