package main

import "encoding/binary"

// imageBounds returns the pixel dimensions from a PNG, JPEG, GIF or WebP
// header without decoding the pixels. It mirrors the bounds-only decode the
// official client uses for its context estimate.
func imageBounds(b []byte) (int, int) {
	if len(b) >= 24 && b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G' {
		return int(binary.BigEndian.Uint32(b[16:20])), int(binary.BigEndian.Uint32(b[20:24]))
	}
	if len(b) >= 10 && b[0] == 'G' && b[1] == 'I' && b[2] == 'F' {
		return int(binary.LittleEndian.Uint16(b[6:8])), int(binary.LittleEndian.Uint16(b[8:10]))
	}
	if len(b) >= 30 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP" {
		switch string(b[12:16]) {
		case "VP8X":
			w := int(b[24]) | int(b[25])<<8 | int(b[26])<<16
			h := int(b[27]) | int(b[28])<<8 | int(b[29])<<16
			return w + 1, h + 1
		case "VP8 ":
			if len(b) >= 30 && b[23] == 0x9d && b[24] == 0x01 && b[25] == 0x2a {
				w := int(binary.LittleEndian.Uint16(b[26:28]) & 0x3fff)
				h := int(binary.LittleEndian.Uint16(b[28:30]) & 0x3fff)
				return w, h
			}
		}
	}
	if len(b) >= 4 && b[0] == 0xff && b[1] == 0xd8 {
		i := 2
		for i+9 < len(b) {
			if b[i] != 0xff {
				i++
				continue
			}
			for i < len(b) && b[i] == 0xff {
				i++
			}
			if i >= len(b) {
				break
			}
			marker := b[i]
			i++
			if marker == 0xd8 || marker == 0xd9 || marker == 0x01 || (marker >= 0xd0 && marker <= 0xd7) {
				continue
			}
			if i+1 >= len(b) {
				break
			}
			segLen := int(binary.BigEndian.Uint16(b[i : i+2]))
			if segLen < 2 {
				break
			}
			if marker >= 0xc0 && marker <= 0xcf && marker != 0xc4 && marker != 0xc8 && marker != 0xcc {
				if i+7 < len(b) {
					h := int(binary.BigEndian.Uint16(b[i+3 : i+5]))
					w := int(binary.BigEndian.Uint16(b[i+5 : i+7]))
					return w, h
				}
				return 0, 0
			}
			i += segLen
		}
	}
	return 0, 0
}
