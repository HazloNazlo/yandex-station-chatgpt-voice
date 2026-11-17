package voice

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	trustedClientToken = "6A5AA1D4EAFF4E9FB37E23D68491D6F4"
	winEpoch           = 11644473600
	secMsGecVersion    = "2-143.0.3650.75"
)

var sha256Initial = [8]uint32{
	0x6A09E667, 0xBB67AE85, 0x3C6EF372, 0xA54FF53A,
	0x510E527F, 0x9B05688C, 0x1F83D9AB, 0x5BE0CD19,
}

var sha256State = [8]uint32{
	0xFD9FBEA2, 0x91243CE7, 0xBE499094, 0xCD2E1E0A,
	0x6C006F49, 0x04BD35AB, 0xFEB0A7E4, 0x03EFFBD2,
}

var sha256Schedule = [14]uint32{
	0xEB24BF9E, 0x1558691F, 0xA0C09EE2, 0x0CFA32BA,
	0xA622EE8C, 0xD1DD9491, 0xC0DD25A0, 0xF9E70AB6,
	0xF0C31573, 0x7058CE4E, 0x7188E606, 0x00E5AB33,
	0xD258B060, 0x72766564,
}

var sha256RoundConstants = [64]uint32{
	0x428A2F98, 0x71374491, 0xB5C0FBCF, 0xE9B5DBA5, 0x3956C25B, 0x59F111F1, 0x923F82A4, 0xAB1C5ED5,
	0xD807AA98, 0x12835B01, 0x243185BE, 0x550C7DC3, 0x72BE5D74, 0x80DEB1FE, 0x9BDC06A7, 0xC19BF174,
	0xE49B69C1, 0xEFBE4786, 0x0FC19DC6, 0x240CA1CC, 0x2DE92C6F, 0x4A7484AA, 0x5CB0A9DC, 0x76F988DA,
	0x983E5152, 0xA831C66D, 0xB00327C8, 0xBF597FC7, 0xC6E00BF3, 0xD5A79147, 0x06CA6351, 0x14292967,
	0x27B70A85, 0x2E1B2138, 0x4D2C6DFC, 0x53380D13, 0x650A7354, 0x766A0ABB, 0x81C2C92E, 0x92722C85,
	0xA2BFE8A1, 0xA81A664B, 0xC24B8B70, 0xC76C51A3, 0xD192E819, 0xD6990624, 0xF40E3585, 0x106AA070,
	0x19A4C116, 0x1E376C08, 0x2748774C, 0x34B0BCB5, 0x391C0CB3, 0x4ED8AA4A, 0x5B9CCA4F, 0x682E6FF3,
	0x748F82EE, 0x78A5636F, 0x84C87814, 0x8CC70208, 0x90BEFFFA, 0xA4506CEB, 0xBEF9A3F7, 0xC67178F2,
}

const (
	sha256Adjustment       = 0xC9DC06A7
	sha256Sigma0Adjustment = 0x059C0B80
	sha256Sigma1Adjustment = 0x000B92C0
	cs64Factor             = 0x82893933
	cs64Adjustment0        = 0xF2F90B1A
	cs64Adjustment1        = 0x0B1A0000
)

var cs64Coefficients = [14][2]uint32{
	{0xE82E91B1, 0xB32700D0},
	{0x44DE24C6, 0x88DFF51D},
	{0x3BEB65BC, 0x29514038},
	{0xC89C5933, 0xB328CBDF},
	{0x8D1EC211, 0xD405B4D5},
	{0x314EF5D5, 0x26E55F3C},
	{0xE92491B1, 0x607801C6},
	{0xF0A5B9D5, 0x82B2C1FB},
	{0x06064CF3, 0xFA51A3DA},
	{0xDA79FA60, 0x207CC464},
	{0xBED20560, 0x3C3C74F3},
	{0x89F9C9F8, 0xD2D14B61},
	{0x35FA38C6, 0xBB04A2C1},
	{0xCB49E3C6, 0xC5626167},
}

func rol16(v uint32) uint32 {
	return (v << 16) | (v >> 16)
}

func wordsLE4Even(data []byte) []uint32 {
	padLen := (4 - (len(data) % 4)) % 4
	padded := append([]byte(nil), data...)
	for i := 0; i < padLen; i++ {
		padded = append(padded, 0)
	}
	if len(padded) == 0 {
		return nil
	}
	words := make([]uint32, len(padded)/4)
	for i := 0; i < len(words); i++ {
		words[i] = binary.LittleEndian.Uint32(padded[i*4 : (i+1)*4])
	}
	if len(words)%2 != 0 {
		words = append(words, 0)
	}
	return words
}

func sigma0(v uint32) uint32 {
	return (v >> 7 | v << 25) ^ (v >> 18 | v << 14) ^ (v >> 3)
}

func sigma1(v uint32) uint32 {
	return (v >> 17 | v << 15) ^ (v >> 19 | v << 13) ^ (v >> 10)
}

func sha256Rounds(state [8]uint32, words []uint32, firstRound int) [8]uint32 {
	a, b, c, d, e, f, g, h := state[0], state[1], state[2], state[3], state[4], state[5], state[6], state[7]
	constants := sha256RoundConstants
	if firstRound == 14 {
		constants[14] = sha256Adjustment
	}

	aligned := ((firstRound + 7) / 8) * 8
	if aligned > 64 {
		aligned = 64
	}

	for idx := firstRound; idx < aligned; idx++ {
		rotE := (e >> 6 | e << 26) ^ (e >> 11 | e << 21) ^ (e >> 25 | e << 7)
		rotA := (a >> 2 | a << 30) ^ (a >> 13 | a << 19) ^ (a >> 22 | a << 10)
		ch := (e & f) ^ (^e & g)
		maj := (a & b) ^ (a & c) ^ (b & c)
		t1 := h + rotE + ch + constants[idx] + words[idx]
		t2 := rotA + maj
		a, b, c, d, e, f, g, h = t1+t2, a, b, c, d+t1, e, f, g
	}

	for idx := aligned; idx < 64; idx += 8 {
		rotE := (e >> 6 | e << 26) ^ (e >> 11 | e << 21) ^ (e >> 25 | e << 7)
		rotA := (a >> 2 | a << 30) ^ (a >> 13 | a << 19) ^ (a >> 22 | a << 10)
		t1 := h + rotE + ((e & f) ^ (^e & g)) + constants[idx] + words[idx]
		d += t1
		h = t1 + rotA + ((a & b) ^ (a & c) ^ (b & c))

		rotE = (d >> 6 | d << 26) ^ (d >> 11 | d << 21) ^ (d >> 25 | d << 7)
		rotA = (h >> 2 | h << 30) ^ (h >> 13 | h << 19) ^ (h >> 22 | h << 10)
		t1 = g + rotE + ((d & e) ^ (^d & f)) + constants[idx+1] + words[idx+1]
		c += t1
		g = t1 + rotA + ((h & a) ^ (h & b) ^ (a & b))

		rotE = (c >> 6 | c << 26) ^ (c >> 11 | c << 21) ^ (c >> 25 | c << 7)
		rotA = (g >> 2 | g << 30) ^ (g >> 13 | g << 19) ^ (g >> 22 | g << 10)
		t1 = f + rotE + ((c & d) ^ (^c & e)) + constants[idx+2] + words[idx+2]
		b += t1
		f = t1 + rotA + ((g & h) ^ (g & a) ^ (h & a))

		rotE = (b >> 6 | b << 26) ^ (b >> 11 | b << 21) ^ (b >> 25 | b << 7)
		rotA = (f >> 2 | f << 30) ^ (f >> 13 | f << 19) ^ (f >> 22 | f << 10)
		t1 = e + rotE + ((b & c) ^ (^b & d)) + constants[idx+3] + words[idx+3]
		a += t1
		e = t1 + rotA + ((f & g) ^ (f & h) ^ (g & h))

		rotE = (a >> 6 | a << 26) ^ (a >> 11 | a << 21) ^ (a >> 25 | a << 7)
		rotA = (e >> 2 | e << 30) ^ (e >> 13 | e << 19) ^ (e >> 22 | e << 10)
		t1 = d + rotE + ((a & b) ^ (^a & c)) + constants[idx+4] + words[idx+4]
		h += t1
		d = t1 + rotA + ((e & f) ^ (e & g) ^ (f & g))

		rotE = (h >> 6 | h << 26) ^ (h >> 11 | h << 21) ^ (h >> 25 | h << 7)
		rotA = (d >> 2 | d << 30) ^ (d >> 13 | d << 19) ^ (d >> 22 | d << 10)
		t1 = c + rotE + ((h & a) ^ (^h & b)) + constants[idx+5] + words[idx+5]
		g += t1
		c = t1 + rotA + ((d & e) ^ (d & f) ^ (e & f))

		rotE = (g >> 6 | g << 26) ^ (g >> 11 | g << 21) ^ (g >> 25 | g << 7)
		rotA = (c >> 2 | c << 30) ^ (c >> 13 | c << 19) ^ (c >> 22 | c << 10)
		t1 = b + rotE + ((g & h) ^ (^g & a)) + constants[idx+6] + words[idx+6]
		f += t1
		b = t1 + rotA + ((c & d) ^ (c & e) ^ (d & e))

		rotE = (f >> 6 | f << 26) ^ (f >> 11 | f << 21) ^ (f >> 25 | f << 7)
		rotA = (b >> 2 | b << 30) ^ (b >> 13 | b << 19) ^ (b >> 22 | b << 10)
		t1 = a + rotE + ((f & g) ^ (^f & h)) + constants[idx+7] + words[idx+7]
		e += t1
		a = t1 + rotA + ((b & c) ^ (b & d) ^ (c & d))
	}
	return [8]uint32{a, b, c, d, e, f, g, h}
}

func expandSha256Schedule(words []uint32, firstWord int) []uint32 {
	res := append([]uint32(nil), words...)
	for idx := firstWord; idx < 64; idx++ {
		x := res[idx-2]
		y := res[idx-15]
		s1 := sigma1(x)
		s0 := sigma0(y)
		res = append(res, s1+res[idx-7]+s0+res[idx-16])
	}
	return res
}

func sha256Init(data []byte) [8]uint32 {
	words := make([]uint32, 14)
	w14 := uint32(data[0])<<16 | uint32(data[1])<<8 | uint32(data[2])
	w15 := binary.BigEndian.Uint32(data[3:7])
	words = append(words, w14, w15)

	for idx := 16; idx < 30; idx++ {
		val := sha256Schedule[idx-16]
		if idx-2 >= 14 {
			if idx == 16 {
				val += sigma1(words[idx-2]) ^ sha256Sigma1Adjustment
			} else {
				val += sigma1(words[idx-2])
			}
		}
		if idx-7 >= 14 {
			val += words[idx-7]
		}
		if idx-15 >= 14 {
			if idx == 29 {
				val += sigma0(words[idx-15]) ^ sha256Sigma0Adjustment
			} else {
				val += sigma0(words[idx-15])
			}
		}
		words = append(words, val)
	}

	w30 := sigma1(words[28]) + words[23] + sigma0(words[15]) + words[14] + sha256Adjustment - sha256RoundConstants[14]
	words = append(words, w30)
	words = expandSha256Schedule(words, 31)

	working := sha256Rounds(sha256State, words, 14)
	var state [8]uint32
	for i := 0; i < 8; i++ {
		state[i] = sha256Initial[i] + working[i]
	}
	return state
}

func sha256Digest(data []byte) []byte {
	length := 57 + len(data)
	padZeros := (55 - length) % 64
	if padZeros < 0 {
		padZeros += 64
	}
	padded := append([]byte(nil), data[7:]...)
	padded = append(padded, 0x80)
	for i := 0; i < padZeros; i++ {
		padded = append(padded, 0)
	}
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(length*8))
	padded = append(padded, lenBuf[:]...)

	state := sha256Init(data[:7])
	for offset := 0; offset < len(padded); offset += 64 {
		var words [16]uint32
		for i := 0; i < 16; i++ {
			words[i] = binary.BigEndian.Uint32(padded[offset+i*4 : offset+(i+1)*4])
		}
		expanded := expandSha256Schedule(words[:], 16)
		working := sha256Rounds(state, expanded, 0)
		for i := 0; i < 8; i++ {
			state[i] += working[i]
		}
	}
	res := make([]byte, 32)
	for i := 0; i < 8; i++ {
		binary.BigEndian.PutUint32(res[i*4:(i+1)*4], state[i])
	}
	return res
}

func cs64Digest(data []byte, digest []byte) (uint32, uint32) {
	keyA := binary.LittleEndian.Uint32(digest[0:4]) | 1
	keyB := binary.LittleEndian.Uint32(digest[4:8]) | 1

	factors := [2]uint32{keyA * cs64Factor, keyB * cs64Factor}
	terms := make([]uint32, 14)
	offsets := make([]uint32, 14)
	for idx := 0; idx < 14; idx++ {
		terms[idx] = cs64Coefficients[idx][0] * factors[idx%2]
		offsets[idx] = cs64Coefficients[idx][1]
	}

	prefixed := append([]byte{0x00}, data...)
	dataWords := wordsLE4Even(prefixed)
	for idx, word := range dataWords {
		var k uint32
		if idx%2 == 0 {
			k = keyA
		} else {
			k = keyB
		}
		term := word * k

		var mul uint32
		if idx%2 == 0 {
			mul = 0xEF0569FB
		} else {
			mul = 0xC31713DB
		}
		offset := rol16(word) * mul

		if idx == 0 {
			term += cs64Adjustment0 * factors[0]
			offset += cs64Adjustment1
		}
		terms = append(terms, term)
		offsets = append(offsets, offset)
	}

	var swapAcc, swapLast, state, acc uint32
	for idx := 0; idx < len(terms); idx += 2 {
		x := terms[idx]
		y := terms[idx+1]
		t := rol16(offsets[idx] + x)
		q := t * 0x76F50F34
		swapAcc += rol16(q)*0xC3EFEA97 + q
		t = rol16(offsets[idx+1] + y)
		q = t * 0x1881CE3C
		swapLast = rol16(q)*0x35BD1EC9 + q
		swapAcc += swapLast

		state = state*keyA + x
		state = rol16(state) * 0xCF98B111
		state = rol16(state) * 0x87085B9F
		state = rol16(state) * 0x12CEB96D
		state = rol16(state) * 0x257E1D83
		acc += state
		t = state*keyB + y
		t = rol16(t) * 0xA27416F5
		t = rol16(t) * 0xD38396FF
		state = rol16(rol16(t)*0x7C932B89) * 0xBFA49F69
		acc += state
	}
	return swapLast ^ state, swapAcc ^ acc
}

var (
	drmMu             sync.RWMutex
	clockSkewSeconds  float64
)

func adjustClockSkew(skew float64) {
	drmMu.Lock()
	clockSkewSeconds += skew
	drmMu.Unlock()
}

func getUnixTimestampWithSkew() float64 {
	drmMu.RLock()
	defer drmMu.RUnlock()
	return float64(time.Now().Unix()) + clockSkewSeconds
}

func generateSecMsGecV2(unixTimestamp float64) string {
	ticks := unixTimestamp + float64(winEpoch)
	ticks -= float64(int64(ticks) % 300)
	ticks *= 1e9 / 100 // 100ns ticks
	strToHash := fmt.Sprintf("%.0f%s", ticks, trustedClientToken)

	digest := sha256Digest([]byte(strToHash))
	w1, w2 := cs64Digest([]byte(strToHash), digest)

	var cs64Buf [8]byte
	binary.LittleEndian.PutUint32(cs64Buf[0:4], w1)
	binary.LittleEndian.PutUint32(cs64Buf[4:8], w2)

	cs64Hex := strings.ToUpper(hex.EncodeToString(cs64Buf[:]))
	sha256Hex := strings.ToUpper(hex.EncodeToString(digest))

	return fmt.Sprintf("%s-%s", cs64Hex, sha256Hex)
}
