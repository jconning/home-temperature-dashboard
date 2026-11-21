package main

import (
	"crypto/aes"
	"crypto/rc4"
)

// Govee thermometer Bluetooth protocol, as worked out by GoveeBTTempLogger
// (github.com/wcbonner/GoveeBTTempLogger) and verified against an H5179.
//
// Every command and reply is a 20-byte packet whose last byte is the XOR of
// bytes 0..18. On devices with the 0x1910 auth service, packets are encrypted:
// AES-128-ECB over bytes 0..15 and RC4 over bytes 16..19, keyed first with the
// pre-shared key below (handshake only) and then with a per-connection session
// key the device hands out during the handshake.

const (
	uuidAuthService = "00010203-0405-0607-0809-0a0b0c0d1910"
	uuidAuthNotify  = "00010203-0405-0607-0809-0a0b0c0d2b10"
	uuidAuthWrite   = "00010203-0405-0607-0809-0a0b0c0d2b11"
	uuidIRService   = "494e5445-4c4c-495f-524f-434b535f4857" // "INTELLI_ROCKS_HW"
	uuidIRDevice    = "494e5445-4c4c-495f-524f-434b535f2011"
	uuidIRCommand   = "494e5445-4c4c-495f-524f-434b535f2012"
	uuidIRData      = "494e5445-4c4c-495f-524f-434b535f2013"

	// maxHistoryMinutes is the most a single history request can ask for
	// (a big-endian uint16 count of minutes back from now, ~45 days).
	maxHistoryMinutes = 0xFFFF
)

// goveePSK is the key used for the auth handshake, from the Govee Home app.
var goveePSK = []byte("MakingLifeSmarte")

// goveePacket builds a 20-byte command with its XOR checksum.
func goveePacket(payload ...byte) []byte {
	p := make([]byte, 20)
	copy(p, payload)
	for _, b := range p[:19] {
		p[19] ^= b
	}
	return p
}

// goveeCrypt encrypts (or decrypts) one 20-byte packet with a 16-byte key.
func goveeCrypt(key, pkt []byte, decrypt bool) []byte {
	out := make([]byte, 20)
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err) // key is always 16 bytes
	}
	if decrypt {
		block.Decrypt(out[:16], pkt[:16])
	} else {
		block.Encrypt(out[:16], pkt[:16])
	}
	c, err := rc4.NewCipher(key)
	if err != nil {
		panic(err)
	}
	c.XORKeyStream(out[16:], pkt[16:20])
	return out
}

// historyRequest asks for the last n minutes of stored readings.
func historyRequest(minutes int) []byte {
	return goveePacket(0x33, 0x01, byte(minutes>>8), byte(minutes), 0x00, 0x01)
}

// bleSample is one stored reading; Offset counts minutes back from the moment
// the history request was sent.
type bleSample struct {
	Offset int
	TempC  float64
	Hum    float64
}

// decodeHistoryPacket decodes a (decrypted) history notification: a big-endian
// offset for the first reading, then up to six 3-byte readings, oldest first.
func decodeHistoryPacket(p []byte) (first int, samples []bleSample) {
	if len(p) < 5 {
		return 0, nil
	}
	first = int(p[0])<<8 | int(p[1])
	off := first
	for i := 2; i+3 <= len(p) && i < 20 && off > 0; i += 3 {
		t, h := decodeReading(p[i], p[i+1], p[i+2])
		samples = append(samples, bleSample{Offset: off, TempC: t, Hum: h})
		off--
	}
	return first, samples
}

// decodeReading unpacks Govee's 3-byte reading: a 24-bit big-endian number n
// where bit 23 is the temperature sign, n/1000 is tenths of a °C and n%1000
// is tenths of a percent humidity. The same packing is used in the sensors'
// live advertisements.
func decodeReading(b0, b1, b2 byte) (tempC, hum float64) {
	n := int(b0)<<16 | int(b1)<<8 | int(b2)
	neg := n&0x800000 != 0
	n &= 0x7FFFF
	tempC = float64(n/1000) / 10
	if neg {
		tempC = -tempC
	}
	return tempC, float64(n%1000) / 10
}
