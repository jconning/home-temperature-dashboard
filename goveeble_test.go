package main

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Captured from the Downstairs H5179 on 2026-11-20: the session key from the
// decrypted handshake reply, and the device's encrypted reply to a history
// request.
func TestGoveeCryptRealCapture(t *testing.T) {
	rx1 := mustHex(t, "e701ee72dd35e430c3233ace09f1f04caf9f0026")
	key := rx1[2:18]
	raw := mustHex(t, "e2c854bdc14544a46bae764894084c5c24a83cd9")
	got := goveeCrypt(key, raw, true)
	if want := mustHex(t, "3301000000000000000000000000000000000032"); !bytes.Equal(got, want) {
		t.Fatalf("decrypt = %x, want %x", got, want)
	}
	if back := goveeCrypt(key, got, false); !bytes.Equal(back, raw) {
		t.Fatalf("re-encrypt = %x, want %x", back, raw)
	}
}

func TestGoveePacketChecksum(t *testing.T) {
	// The device's own reply carries the same checksum scheme.
	if got := goveePacket(0x33, 0x01); got[19] != 0x32 {
		t.Fatalf("checksum = %#x, want 0x32", got[19])
	}
	req := historyRequest(200)
	if !bytes.Equal(req[:6], []byte{0x33, 0x01, 0x00, 0xc8, 0x00, 0x01}) || len(req) != 20 {
		t.Fatalf("request = %x", req)
	}
}

func TestDecodeHistoryPacket(t *testing.T) {
	// First decrypted data packet of a 200-minute request.
	first, s := decodeHistoryPacket(mustHex(t, "00c80355b10355af0355ae0351c60355ac0355ad"))
	if first != 200 || len(s) != 6 {
		t.Fatalf("first=%d n=%d", first, len(s))
	}
	if s[0].Offset != 200 || s[0].TempC != 21.8 || s[0].Hum != 54.5 {
		t.Errorf("s[0] = %+v", s[0])
	}
	if s[3].Offset != 197 || s[3].TempC != 21.7 || s[3].Hum != 54.2 {
		t.Errorf("s[3] = %+v", s[3])
	}
	// Last packet: offset 2 means only two readings remain.
	_, s = decodeHistoryPacket(mustHex(t, "00020355b10355af0000000000000000000000ff"))
	if len(s) != 2 || s[1].Offset != 1 {
		t.Errorf("tail = %+v", s)
	}
}

func TestDecodeReadingNegative(t *testing.T) {
	// -5.3 °C, 41.0 %: 53410 with the sign bit set.
	n := 0x800000 | 53410
	if tc, h := decodeReading(byte(n>>16), byte(n>>8), byte(n)); tc != -5.3 || h != 41.0 {
		t.Fatalf("got %v %v", tc, h)
	}
}

func TestBLEName(t *testing.T) {
	d := Device{ID: "A1:B2:C3:D4:E5:F6:1A:2B", SKU: "H5179"}
	for _, n := range []string{"GV5179_1A2B", "GVH5179_1A2B", "Govee_H5179_1A2B"} {
		if !bleNameMatches(d, n) {
			t.Errorf("%s should match", n)
		}
	}
	for _, n := range []string{"GV5179_3C4D", "GV5075_1A2B", ""} {
		if bleNameMatches(d, n) {
			t.Errorf("%s should not match", n)
		}
	}
}
