package client

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/smallfishsworld/Real-Time-Video-Conferencing-System/server/internal/protocol"
)

// The packetizer that the Go test clients and the load generator share must lay out
// frames exactly like the reference encoder (protocol-testdata/generate_vectors.py)
// and the Python client.
func TestPacketizerMatchesTheFrameVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "protocol-testdata", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Frames []struct {
			Name       string   `json:"name"`
			SSRC       uint32   `json:"ssrc"`
			FirstSeq   uint16   `json:"first_seq"`
			FrameID    uint32   `json:"frame_id"`
			Timestamp  uint32   `json:"timestamp"`
			FECK       int      `json:"fec_k"`
			Size       int      `json:"size"`
			PacketsHex []string `json:"packets_hex"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Frames) == 0 {
		t.Fatal("no frame vectors")
	}
	for _, v := range file.Frames {
		t.Run(v.Name, func(t *testing.T) {
			data := make([]byte, v.Size)
			for i := range data {
				data[i] = byte(i*7 + 3)
			}
			p := Packetizer{SSRC: v.SSRC, Seq: v.FirstSeq}
			var enc protocol.FECEncoder
			got := p.VideoFrame(nil, data, v.FrameID, v.Timestamp, v.FECK, &enc)
			if len(got) != len(v.PacketsHex) {
				t.Fatalf("%d packets, want %d", len(got), len(v.PacketsHex))
			}
			for i, pkt := range got {
				if h := hex.EncodeToString(pkt); h != v.PacketsHex[i] {
					t.Errorf("packet %d:\n got %s\nwant %s", i, h, v.PacketsHex[i])
				}
			}
			if want := v.FirstSeq + uint16(len(got)); p.Seq != want {
				t.Errorf("next sequence number %d, want %d", p.Seq, want)
			}
		})
	}
}
