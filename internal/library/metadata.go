package library

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sort"
)

type imageMetadata struct {
	EXIF, ICC []byte
	Frames    int
}

const maxMetadata = 16 << 20

func readMetadata(path string) (imageMetadata, error) {
	m := imageMetadata{Frames: 1}
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer f.Close()
	var head [12]byte
	n, _ := io.ReadFull(f, head[:])
	f.Seek(0, 0)
	read := func(n int64) ([]byte, error) {
		if n < 0 || n > maxMetadata {
			return nil, fmt.Errorf("метаданные превышают лимит 16 МБ")
		}
		b := make([]byte, n)
		_, e := io.ReadFull(f, b)
		return b, e
	}
	if n >= 8 && (string(head[:4]) == "II*\x00" || string(head[:4]) == "MM\x00*") {
		return readTIFFMetadata(f, head[:8])
	}
	if n >= 2 && string(head[:2]) == "BM" {
		f.Seek(14, 0)
		var size [4]byte
		if _, err = io.ReadFull(f, size[:]); err != nil {
			return m, err
		}
		if binary.LittleEndian.Uint32(size[:]) >= 124 {
			f.Seek(14, 0)
			header, e := read(124)
			if e != nil {
				return m, e
			}
			kind := binary.LittleEndian.Uint32(header[56:])
			amount := binary.LittleEndian.Uint32(header[116:])
			if kind == 0x4d424544 && amount > 0 {
				if _, e = f.Seek(14+int64(binary.LittleEndian.Uint32(header[112:])), 0); e != nil {
					return m, e
				}
				m.ICC, e = read(int64(amount))
				if e != nil {
					return m, e
				}
			} else if kind == 0x4c494e4b {
				return m, fmt.Errorf("BMP с внешним цветовым профилем не поддерживается")
			}
		}
		return m, nil
	}
	if n >= 2 && head[0] == 255 && head[1] == 216 {
		f.Seek(2, 0)
		segments := map[int][]byte{}
		for {
			var marker [2]byte
			if _, err = io.ReadFull(f, marker[:]); err != nil {
				return m, err
			}
			if marker[0] != 255 {
				return m, fmt.Errorf("повреждённый JPEG")
			}
			if marker[1] == 0xda || marker[1] == 0xd9 {
				break
			}
			if marker[1] == 0xff {
				f.Seek(-1, 1)
				continue
			}
			if marker[1] >= 0xd0 && marker[1] <= 0xd7 {
				continue
			}
			var size [2]byte
			if _, err = io.ReadFull(f, size[:]); err != nil {
				return m, err
			}
			length := int64(binary.BigEndian.Uint16(size[:])) - 2
			if length < 0 {
				return m, fmt.Errorf("повреждённый JPEG")
			}
			if marker[1] == 0xe1 || marker[1] == 0xe2 {
				data, e := read(length)
				if e != nil {
					return m, e
				}
				if bytes.HasPrefix(data, []byte("Exif\x00\x00")) {
					m.EXIF = append([]byte{}, data[6:]...)
				}
				if bytes.HasPrefix(data, []byte("ICC_PROFILE\x00")) && len(data) >= 14 {
					segments[int(data[12])] = data[14:]
				}
			} else {
				if _, err = f.Seek(length, 1); err != nil {
					return m, err
				}
			}
		}
		keys := []int{}
		for k := range segments {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		for _, k := range keys {
			m.ICC = append(m.ICC, segments[k]...)
			if len(m.ICC) > maxMetadata {
				return m, fmt.Errorf("цветовой профиль слишком велик")
			}
		}
		return m, nil
	}
	if n >= 8 && bytes.Equal(head[:8], []byte("\x89PNG\r\n\x1a\n")) {
		f.Seek(8, 0)
		for {
			var hdr [8]byte
			if _, err = io.ReadFull(f, hdr[:]); err != nil {
				return m, err
			}
			length := int64(binary.BigEndian.Uint32(hdr[:4]))
			name := string(hdr[4:])
			switch name {
			case "eXIf", "iCCP", "acTL":
				data, e := read(length)
				if e != nil {
					return m, e
				}
				switch name {
				case "eXIf":
					m.EXIF = data
				case "acTL":
					if len(data) >= 4 {
						m.Frames = int(binary.BigEndian.Uint32(data[:4]))
					}
				case "iCCP":
					i := bytes.IndexByte(data, 0)
					if i < 0 || i+2 > len(data) {
						return m, fmt.Errorf("повреждённый ICC PNG")
					}
					zr, e := zlib.NewReader(bytes.NewReader(data[i+2:]))
					if e != nil {
						return m, e
					}
					m.ICC, e = io.ReadAll(io.LimitReader(zr, maxMetadata+1))
					zr.Close()
					if e != nil {
						return m, e
					}
					if len(m.ICC) > maxMetadata {
						return m, fmt.Errorf("цветовой профиль слишком велик")
					}
				}
			default:
				if _, err = f.Seek(length, 1); err != nil {
					return m, err
				}
			}
			if _, err = f.Seek(4, 1); err != nil {
				return m, err
			}
			if name == "IEND" {
				return m, nil
			}
		}
	}
	if n >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP" {
		f.Seek(12, 0)
		frames := 0
		for {
			var hdr [8]byte
			if _, err = io.ReadFull(f, hdr[:]); err == io.EOF {
				return m, nil
			} else if err != nil {
				return m, err
			}
			length := int64(binary.LittleEndian.Uint32(hdr[4:]))
			name := string(hdr[:4])
			if name == "ANMF" {
				frames++
				m.Frames = max(2, frames)
			}
			if name == "EXIF" || name == "ICCP" {
				data, e := read(length)
				if e != nil {
					return m, e
				}
				if name == "EXIF" {
					m.EXIF = bytes.TrimPrefix(data, []byte("Exif\x00\x00"))
				} else {
					m.ICC = data
				}
			} else {
				if _, err = f.Seek(length, 1); err != nil {
					return m, err
				}
			}
			if length&1 != 0 {
				f.Seek(1, 1)
			}
		}
	}
	return m, nil
}

func readTIFFMetadata(f *os.File, header []byte) (imageMetadata, error) {
	m := imageMetadata{Frames: 0}
	order := exifOrder(header)
	if order == nil {
		return m, fmt.Errorf("недопустимый TIFF")
	}
	st, err := f.Stat()
	if err != nil {
		return m, err
	}
	next := order.Uint32(header[4:])
	visited := map[uint32]bool{}
	orientation := uint16(1)
	for next != 0 {
		if visited[next] || int64(next)+2 > st.Size() || m.Frames > 100000 {
			return m, fmt.Errorf("повреждённый список страниц TIFF")
		}
		visited[next] = true
		if _, err = f.Seek(int64(next), 0); err != nil {
			return m, err
		}
		var countBytes [2]byte
		if _, err = io.ReadFull(f, countBytes[:]); err != nil {
			return m, err
		}
		count := int(order.Uint16(countBytes[:]))
		data := make([]byte, count*12+4)
		if _, err = io.ReadFull(f, data); err != nil {
			return m, err
		}
		if m.Frames == 0 {
			for i := 0; i < count; i++ {
				entry := data[i*12 : (i+1)*12]
				tag, typ, amount := order.Uint16(entry), order.Uint16(entry[2:]), order.Uint32(entry[4:])
				if tag == 274 && typ == 3 && amount == 1 {
					orientation = order.Uint16(entry[8:])
				}
				if tag == 34675 && amount > 0 {
					if amount > maxMetadata {
						return m, fmt.Errorf("цветовой профиль TIFF слишком велик")
					}
					m.ICC = make([]byte, amount)
					if amount <= 4 {
						copy(m.ICC, entry[8:])
					} else {
						if _, err = f.Seek(int64(order.Uint32(entry[8:])), 0); err != nil {
							return m, err
						}
						if _, err = io.ReadFull(f, m.ICC); err != nil {
							return m, err
						}
					}
				}
			}
		}
		next = order.Uint32(data[count*12:])
		m.Frames++
	}
	m.Frames = max(1, m.Frames)
	m.EXIF = []byte{'I', 'I', 42, 0, 8, 0, 0, 0, 1, 0, 0x12, 1, 3, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(m.EXIF[18:], orientation)
	return m, nil
}
func exifOrder(data []byte) binary.ByteOrder {
	if len(data) < 8 {
		return nil
	}
	if string(data[:2]) == "II" {
		return binary.LittleEndian
	}
	if string(data[:2]) == "MM" {
		return binary.BigEndian
	}
	return nil
}
func exifValue(data []byte, tag uint16) int {
	order := exifOrder(data)
	if order == nil {
		return 1
	}
	offset := int(order.Uint32(data[4:8]))
	if offset < 0 || offset+2 > len(data) {
		return 1
	}
	count := int(order.Uint16(data[offset:]))
	for i := 0; i < count; i++ {
		p := offset + 2 + i*12
		if p+12 > len(data) {
			break
		}
		if order.Uint16(data[p:]) == tag {
			v := int(order.Uint16(data[p+8:]))
			if v >= 1 && v <= 8 {
				return v
			}
		}
	}
	return 1
}
func patchExif(data []byte, w, h int) {
	order := exifOrder(data)
	if order == nil {
		return
	}
	var walk func(int, int)
	walk = func(offset, depth int) {
		if depth > 3 || offset < 0 || offset+2 > len(data) {
			return
		}
		count := int(order.Uint16(data[offset:]))
		for i := 0; i < count; i++ {
			p := offset + 2 + i*12
			if p+12 > len(data) {
				return
			}
			tag, typ := order.Uint16(data[p:]), order.Uint16(data[p+2:])
			if tag == 0x8769 {
				walk(int(order.Uint32(data[p+8:])), depth+1)
			}
			value := 0
			switch tag {
			case 274:
				value = 1
			case 256, 0xa002:
				value = w
			case 257, 0xa003:
				value = h
			}
			if value > 0 && order.Uint32(data[p+4:]) == 1 {
				if typ == 3 && value <= 65535 {
					order.PutUint16(data[p+8:], uint16(value))
				} else if typ == 4 {
					order.PutUint32(data[p+8:], uint32(value))
				}
			}
		}
	}
	walk(int(order.Uint32(data[4:])), 0)
}

func writeMetadata(path, format string, m imageMetadata, w, h int) error {
	if len(m.ICC) == 0 && len(m.EXIF) == 0 {
		return nil
	}
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp := path + ".metadata"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { dst.Close(); os.Remove(tmp) }()
	write := func(b []byte) error { _, e := dst.Write(b); return e }
	switch format {
	case "jpg":
		prefix := make([]byte, 2)
		if _, err = io.ReadFull(src, prefix); err != nil {
			return err
		}
		if err = write(prefix); err != nil {
			return err
		}
		segment := func(marker byte, data []byte) error {
			if len(data) > 65533 {
				return fmt.Errorf("EXIF не помещается в JPEG")
			}
			header := []byte{255, marker, 0, 0}
			binary.BigEndian.PutUint16(header[2:], uint16(len(data)+2))
			if e := write(header); e != nil {
				return e
			}
			return write(data)
		}
		if len(m.EXIF) > 0 {
			if err = segment(0xe1, append([]byte("Exif\x00\x00"), m.EXIF...)); err != nil {
				return err
			}
		}
		chunks := (len(m.ICC) + 65518) / 65519
		if chunks > 255 {
			return fmt.Errorf("цветовой профиль не помещается в JPEG")
		}
		for i := 0; i < chunks; i++ {
			data := append([]byte("ICC_PROFILE\x00"), byte(i+1), byte(chunks))
			data = append(data, m.ICC[i*65519:min((i+1)*65519, len(m.ICC))]...)
			if err = segment(0xe2, data); err != nil {
				return err
			}
		}
	case "png":
		prefix := make([]byte, 33)
		if _, err = io.ReadFull(src, prefix); err != nil {
			return err
		}
		if err = write(prefix); err != nil {
			return err
		}
		chunk := func(name string, data []byte) error {
			header := make([]byte, 8)
			binary.BigEndian.PutUint32(header, uint32(len(data)))
			copy(header[4:], name)
			if e := write(header); e != nil {
				return e
			}
			if e := write(data); e != nil {
				return e
			}
			checksum := crc32.NewIEEE()
			checksum.Write([]byte(name))
			checksum.Write(data)
			var tail [4]byte
			binary.BigEndian.PutUint32(tail[:], checksum.Sum32())
			return write(tail[:])
		}
		if len(m.ICC) > 0 {
			var zipped bytes.Buffer
			zw := zlib.NewWriter(&zipped)
			zw.Write(m.ICC)
			if err = zw.Close(); err != nil {
				return err
			}
			if err = chunk("iCCP", append([]byte("ICC\x00\x00"), zipped.Bytes()...)); err != nil {
				return err
			}
		}
		if len(m.EXIF) > 0 {
			if err = chunk("eXIf", m.EXIF); err != nil {
				return err
			}
		}
	case "webp":
		var prefix [12]byte
		if _, err = io.ReadFull(src, prefix[:]); err != nil {
			return err
		}
		info, err := src.Stat()
		if err != nil {
			return err
		}
		var hdr [8]byte
		if _, err = io.ReadFull(src, hdr[:]); err != nil {
			return err
		}
		flags := byte(0)
		skip := int64(0)
		if string(hdr[:4]) == "VP8X" {
			payload := make([]byte, 10)
			if _, err = io.ReadFull(src, payload); err != nil {
				return err
			}
			flags = payload[0]
			skip = 18
		} else {
			var bits [5]byte
			if _, err = io.ReadFull(src, bits[:]); err != nil {
				return err
			}
			if string(hdr[:4]) == "VP8L" && bits[4]&16 != 0 {
				flags |= 16
			}
		}
		src.Seek(12+skip, 0)
		if len(m.ICC) > 0 {
			flags |= 32
		}
		if len(m.EXIF) > 0 {
			flags |= 8
		}
		length := info.Size() + 18 - skip
		for _, data := range [][]byte{m.ICC, m.EXIF} {
			if len(data) > 0 {
				length += int64(8 + len(data) + (len(data) & 1))
			}
		}
		binary.LittleEndian.PutUint32(prefix[4:8], uint32(length-8))
		if err = write(prefix[:]); err != nil {
			return err
		}
		chunk := func(name string, data []byte) error {
			header := make([]byte, 8)
			copy(header, name)
			binary.LittleEndian.PutUint32(header[4:], uint32(len(data)))
			if e := write(header); e != nil {
				return e
			}
			if e := write(data); e != nil {
				return e
			}
			if len(data)&1 != 0 {
				return write([]byte{0})
			}
			return nil
		}
		extended := make([]byte, 10)
		extended[0] = flags
		for i := 0; i < 3; i++ {
			extended[4+i] = byte((w - 1) >> (8 * i))
			extended[7+i] = byte((h - 1) >> (8 * i))
		}
		if err = chunk("VP8X", extended); err != nil {
			return err
		}
		if len(m.ICC) > 0 {
			if err = chunk("ICCP", m.ICC); err != nil {
				return err
			}
		}
		if _, err = io.Copy(dst, src); err != nil {
			return err
		}
		if len(m.EXIF) > 0 {
			if err = chunk("EXIF", m.EXIF); err != nil {
				return err
			}
		}
		src.Seek(0, io.SeekEnd)
	}
	if _, err = io.Copy(dst, src); err != nil {
		return err
	}
	if err = dst.Sync(); err != nil {
		return err
	}
	if err = dst.Close(); err != nil {
		return err
	}
	src.Close()
	if err = os.Remove(path); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
