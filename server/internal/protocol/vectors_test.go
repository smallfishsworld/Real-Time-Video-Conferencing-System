package protocol

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// The golden vectors are shared with the Python client tests; see
// protocol-testdata/generate_vectors.py for how they were produced.
type vectorFile struct {
	RTP []struct {
		Name       string `json:"name"`
		Hex        string `json:"hex"`
		DecodeOnly bool   `json:"decode_only"`
		Header     struct {
			Marker bool     `json:"marker"`
			PT     uint8    `json:"pt"`
			Seq    uint16   `json:"seq"`
			TS     uint32   `json:"ts"`
			SSRC   uint32   `json:"ssrc"`
			CSRC   []uint32 `json:"csrc"`
		} `json:"header"`
		Video *struct {
			FrameID uint32 `json:"frame_id"`
			Index   uint16 `json:"index"`
			Count   uint16 `json:"count"`
		} `json:"video"`
		PayloadHex string `json:"payload_hex"`
	} `json:"rtp"`
	FEC []struct {
		Name       string   `json:"name"`
		PacketsHex []string `json:"packets_hex"`
		ParitySeq  uint16   `json:"parity_seq"`
		ParityHex  string   `json:"parity_hex"`
	} `json:"fec"`
	Register []struct {
		Name  string `json:"name"`
		Hex   string `json:"hex"`
		TxID  uint64 `json:"txid"`
		Token string `json:"token"`
		Addr  string `json:"addr"`
	} `json:"register"`
	Control []struct {
		Name    string         `json:"name"`
		Hex     string         `json:"hex"`
		Message map[string]any `json:"message"`
	} `json:"control"`
	NACK []struct {
		Name       string   `json:"name"`
		Hex        string   `json:"hex"`
		SenderSSRC uint32   `json:"sender_ssrc"`
		MediaSSRC  uint32   `json:"media_ssrc"`
		Lost       []uint16 `json:"lost"`
	} `json:"nack"`
	Invalid []struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
		Hex  string `json:"hex"`
	} `json:"invalid"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "protocol-testdata", "vectors.json"))
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var v vectorFile
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	return v
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func TestGoldenRTP(t *testing.T) {
	for _, vec := range loadVectors(t).RTP {
		t.Run(vec.Name, func(t *testing.T) {
			pkt := unhex(t, vec.Hex)
			var h RTPHeader
			payload, err := ParseRTP(pkt, &h)
			if err != nil {
				t.Fatalf("ParseRTP: %v", err)
			}
			want := vec.Header
			if h.Marker != want.Marker || h.PayloadType != want.PT || h.SequenceNumber != want.Seq ||
				h.Timestamp != want.TS || h.SSRC != want.SSRC || int(h.CSRCCount) != len(want.CSRC) {
				t.Fatalf("header mismatch: got %+v want %+v", h, want)
			}
			for i, c := range want.CSRC {
				if h.CSRC[i] != c {
					t.Fatalf("csrc[%d] = %d, want %d", i, h.CSRC[i], c)
				}
			}
			body := payload
			if vec.Video != nil {
				var v VideoHeader
				body, err = ParseVideo(payload, &v)
				if err != nil {
					t.Fatalf("ParseVideo: %v", err)
				}
				if v.FrameID != vec.Video.FrameID || v.FragmentIndex != vec.Video.Index || v.FragmentCount != vec.Video.Count {
					t.Fatalf("video header mismatch: %+v", v)
				}
			}
			if !bytes.Equal(body, unhex(t, vec.PayloadHex)) {
				t.Fatalf("payload mismatch")
			}
			if vec.DecodeOnly {
				return
			}
			out := make([]byte, 1500)
			n, err := h.MarshalTo(out)
			if err != nil {
				t.Fatalf("MarshalTo: %v", err)
			}
			if vec.Video != nil {
				v := VideoHeader{FrameID: vec.Video.FrameID, FragmentIndex: vec.Video.Index, FragmentCount: vec.Video.Count}
				m, err := v.MarshalTo(out[n:])
				if err != nil {
					t.Fatalf("video MarshalTo: %v", err)
				}
				n += m
			}
			n += copy(out[n:], body)
			if !bytes.Equal(out[:n], pkt) {
				t.Fatalf("encode mismatch:\n got %x\nwant %x", out[:n], pkt)
			}
		})
	}
}

func TestGoldenFEC(t *testing.T) {
	for _, vec := range loadVectors(t).FEC {
		t.Run(vec.Name, func(t *testing.T) {
			pkts := make([][]byte, len(vec.PacketsHex))
			var enc FECEncoder
			for i, h := range vec.PacketsHex {
				pkts[i] = unhex(t, h)
				if err := enc.Add(pkts[i]); err != nil {
					t.Fatalf("Add: %v", err)
				}
			}
			var rh RTPHeader
			if _, err := ParseRTP(pkts[0], &rh); err != nil {
				t.Fatal(err)
			}
			out := make([]byte, MaxSendDatagram)
			n, err := enc.Flush(out, rh.SSRC, vec.ParitySeq)
			if err != nil {
				t.Fatalf("Flush: %v", err)
			}
			parity := unhex(t, vec.ParityHex)
			if !bytes.Equal(out[:n], parity) {
				t.Fatalf("parity mismatch:\n got %x\nwant %x", out[:n], parity)
			}
			for missing := range pkts {
				var rec FECRecovery
				if err := rec.Start(parity); err != nil {
					t.Fatalf("Start: %v", err)
				}
				for i, p := range pkts {
					if i != missing {
						if err := rec.Add(p); err != nil {
							t.Fatalf("Add: %v", err)
						}
					}
				}
				var mh RTPHeader
				if _, err := ParseRTP(pkts[missing], &mh); err != nil {
					t.Fatal(err)
				}
				m, err := rec.Finish(out, mh.SequenceNumber)
				if err != nil {
					t.Fatalf("Finish: %v", err)
				}
				if !bytes.Equal(out[:m], pkts[missing]) {
					t.Fatalf("recovered packet %d mismatch:\n got %x\nwant %x", missing, out[:m], pkts[missing])
				}
			}
		})
	}
}

func TestGoldenRegister(t *testing.T) {
	for _, vec := range loadVectors(t).Register {
		t.Run(vec.Name, func(t *testing.T) {
			b := unhex(t, vec.Hex)
			out := make([]byte, 64)
			if vec.Token != "" {
				var r Register
				if err := ParseRegister(b, &r); err != nil {
					t.Fatalf("ParseRegister: %v", err)
				}
				tok, err := ParseToken(vec.Token)
				if err != nil {
					t.Fatal(err)
				}
				if r.TxID != vec.TxID || r.Token != tok {
					t.Fatalf("got %+v", r)
				}
				n, _ := r.MarshalTo(out)
				if !bytes.Equal(out[:n], b) {
					t.Fatalf("encode mismatch %x", out[:n])
				}
				return
			}
			var a RegisterAck
			if err := ParseRegisterAck(b, &a); err != nil {
				t.Fatalf("ParseRegisterAck: %v", err)
			}
			if a.TxID != vec.TxID || a.Observed != netip.MustParseAddrPort(vec.Addr) {
				t.Fatalf("got %+v", a)
			}
			n, err := a.MarshalTo(out)
			if err != nil || !bytes.Equal(out[:n], b) {
				t.Fatalf("encode mismatch %x (%v)", out[:n], err)
			}
		})
	}
}

func TestGoldenControl(t *testing.T) {
	for _, vec := range loadVectors(t).Control {
		t.Run(vec.Name, func(t *testing.T) {
			raw := unhex(t, vec.Hex)
			body, err := ReadFrame(bytes.NewReader(raw), nil)
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, vec.Message) {
				t.Fatalf("decoded %v, want %v", got, vec.Message)
			}
			// Re-encoding with the Go encoder must yield the same JSON value.
			frame, err := EncodeFrame(vec.Message)
			if err != nil {
				t.Fatal(err)
			}
			body2, err := ReadFrame(bytes.NewReader(frame), nil)
			if err != nil {
				t.Fatal(err)
			}
			var again map[string]any
			if err := json.Unmarshal(body2, &again); err != nil || !reflect.DeepEqual(again, vec.Message) {
				t.Fatalf("round trip mismatch: %v (%v)", again, err)
			}
		})
	}
}

func TestGoldenNACK(t *testing.T) {
	vecs := loadVectors(t).NACK
	if len(vecs) == 0 {
		t.Fatal("no NACK vectors")
	}
	for _, vec := range vecs {
		t.Run(vec.Name, func(t *testing.T) {
			b := unhex(t, vec.Hex)
			var n NACK
			if err := ParseNACK(b, &n); err != nil {
				t.Fatal(err)
			}
			if n.SenderSSRC != vec.SenderSSRC || n.MediaSSRC != vec.MediaSSRC || !slices.Equal(n.AppendLost(nil), vec.Lost) {
				t.Fatalf("got %+v, lost %v; want %v", n, n.AppendLost(nil), vec.Lost)
			}
			var m NACK
			m.SenderSSRC, m.MediaSSRC = vec.SenderSSRC, vec.MediaSSRC
			m.SetLost(vec.Lost)
			buf := make([]byte, len(b))
			if size, err := m.MarshalTo(buf); err != nil || !bytes.Equal(buf[:size], b) {
				t.Fatalf("SetLost+MarshalTo = %x, %v; want %x", buf[:size], err, b)
			}
		})
	}
}

func TestGoldenInvalid(t *testing.T) {
	for _, vec := range loadVectors(t).Invalid {
		t.Run(vec.Kind+"/"+vec.Name, func(t *testing.T) {
			b := unhex(t, vec.Hex)
			var err error
			switch vec.Kind {
			case "rtp":
				var h RTPHeader
				_, err = ParseRTP(b, &h)
			case "video":
				var v VideoHeader
				_, err = ParseVideo(b, &v)
			case "fec":
				var f FECHeader
				_, err = ParseFEC(b, &f)
			case "register":
				var r Register
				err = ParseRegister(b, &r)
			case "nack":
				var n NACK
				err = ParseNACK(b, &n)
			case "control":
				var body []byte
				body, err = ReadFrame(bytes.NewReader(b), nil)
				if err == nil {
					_, err = DecodeRequest(body)
				}
			default:
				t.Fatalf("unknown kind %q", vec.Kind)
			}
			if err == nil {
				t.Fatalf("expected an error for %s", vec.Name)
			}
		})
	}
}
