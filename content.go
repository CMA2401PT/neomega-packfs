package packfs

import (
	"encoding/binary"
	"io"
)

const rootID uint64 = 1

// object 用长度决定存储档位，目录的父 ID 用于阻止循环重命名。
type object struct {
	id       uint64
	parent   uint64
	dir      bool
	size     uint32
	inline   []byte
	direct   [8]uint32
	indirect uint32
	linked   bool
}

func decodeObject(id uint64, b []byte) *object {
	o := &object{id: id, dir: b[0] == 1, parent: binary.LittleEndian.Uint64(b[1:9]), size: get32(b, 9), linked: true}
	switch {
	case o.size <= 512:
		o.inline = append([]byte(nil), b[13:]...)
	case o.size <= 8*blockSize:
		for i := range o.direct {
			o.direct[i] = get32(b, 13+i*4)
		}
	default:
		o.indirect = get32(b, 13)
	}
	return o
}

func (o *object) encode() []byte {
	size := 13
	switch {
	case o.size <= 512:
		size += int(o.size)
	case o.size <= 8*blockSize:
		size += 32
	default:
		size += 4
	}
	b := make([]byte, size)
	if o.dir {
		b[0] = 1
	}
	binary.LittleEndian.PutUint64(b[1:9], o.parent)
	put32(b, 9, o.size)
	switch {
	case o.size <= 512:
		copy(b[13:], o.inline)
	case o.size <= 8*blockSize:
		for i, id := range o.direct {
			put32(b, 13+i*4, id)
		}
	default:
		put32(b, 13, o.indirect)
	}
	return b
}

func (s *blockStore) dataID(o *object, index uint32) (uint32, error) {
	if o.size <= 8*blockSize {
		return o.direct[index], nil
	}
	if o.indirect == 0 {
		return 0, nil
	}
	a, err := s.read(o.indirect)
	if err != nil {
		return 0, err
	}
	second := get32(a, int(index/1024)*4)
	if second == 0 {
		return 0, nil
	}
	b, err := s.read(second)
	if err != nil {
		return 0, err
	}
	return get32(b, int(index%1024)*4), nil
}

func (s *blockStore) setDataID(o *object, index, id uint32) error {
	if o.size <= 8*blockSize {
		o.direct[index] = id
		return nil
	}
	first, a, err := s.cow(o.indirect)
	if err != nil {
		return err
	}
	o.indirect = first
	pos := int(index/1024) * 4
	second, b, err := s.cow(get32(a, pos))
	if err != nil {
		return err
	}
	put32(a, pos, second)
	put32(b, int(index%1024)*4, id)
	return nil
}

func (s *blockStore) readContent(o *object, dst []byte, off int64) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	if off >= int64(o.size) {
		return 0, io.EOF
	}
	limit := len(dst)
	if int64(limit) > int64(o.size)-off {
		limit = int(int64(o.size) - off)
	}
	if o.size <= 512 {
		copy(dst[:limit], o.inline[int(off):int(off)+limit])
		if limit < len(dst) {
			return limit, io.EOF
		}
		return limit, nil
	}
	n := 0
	for n < limit {
		pos := off + int64(n)
		index := uint32(pos / blockSize)
		start := int(pos % blockSize)
		count := min(limit-n, blockSize-start)
		id, err := s.dataID(o, index)
		if err != nil {
			return n, err
		}
		if id == 0 {
			clear(dst[n : n+count])
		} else {
			b, err := s.read(id)
			if err != nil {
				return n, err
			}
			copy(dst[n:n+count], b[start:start+count])
		}
		n += count
	}
	if limit < len(dst) {
		return n, io.EOF
	}
	return n, nil
}

// resizeContent 在缩小时清掉末块尾部，并撤下多余映射，保证重新扩展只能读到零。
func (s *blockStore) resizeContent(o *object, size uint32) error {
	oldSize := o.size
	if size == oldSize {
		return nil
	}
	if size <= 512 {
		b := make([]byte, int(size))
		if size != 0 {
			_, err := s.readContent(o, b[:min(size, oldSize)], 0)
			if err != nil {
				return err
			}
		}
		if err := s.freeContent(o); err != nil {
			return err
		}
		o.size = size
		o.inline = b
		o.direct = [8]uint32{}
		o.indirect = 0
		return nil
	}
	if oldSize <= 512 {
		old := o.inline
		o.inline = nil
		o.size = size
		if len(old) != 0 {
			id, err := s.alloc(old)
			if err != nil {
				return err
			}
			if err := s.setDataID(o, 0, id); err != nil {
				return err
			}
		}
		return nil
	}
	if size < oldSize && size%blockSize != 0 {
		index := size / blockSize
		id, err := s.dataID(o, index)
		if err != nil {
			return err
		}
		if id != 0 {
			next, b, err := s.cow(id)
			if err != nil {
				return err
			}
			clear(b[int(size%blockSize):])
			if err := s.setDataID(o, index, next); err != nil {
				return err
			}
		}
	}
	if oldSize <= 8*blockSize && size > 8*blockSize {
		direct := o.direct
		o.direct = [8]uint32{}
		o.size = size
		for i, id := range direct {
			if id != 0 {
				if err := s.setDataID(o, uint32(i), id); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if oldSize > 8*blockSize && size <= 8*blockSize {
		keep := (size + blockSize - 1) / blockSize
		for i := uint32(0); i < keep; i++ {
			id, err := s.dataID(o, i)
			if err != nil {
				return err
			}
			o.direct[i] = id
		}
		if err := s.freeIndirect(o.indirect, keep); err != nil {
			return err
		}
		o.indirect = 0
	} else if size < oldSize && o.indirect != 0 {
		keep := (uint64(size) + blockSize - 1) / blockSize
		first, a, err := s.cow(o.indirect)
		if err != nil {
			return err
		}
		o.indirect = first
		for i := 0; i < 1024; i++ {
			id := get32(a, i*4)
			if id == 0 {
				continue
			}
			if uint64(i)*1024 >= keep {
				if err := s.freeSecond(id, 0); err != nil {
					return err
				}
				put32(a, i*4, 0)
				continue
			}
			if uint64(i+1)*1024 > keep {
				second, b, err := s.cow(id)
				if err != nil {
					return err
				}
				put32(a, i*4, second)
				for j := int(keep % 1024); j < 1024; j++ {
					s.free(get32(b, j*4))
				}
				clear(b[int(keep%1024)*4:])
				if allZero(b) {
					s.free(second)
					put32(a, i*4, 0)
				}
			}
		}
		if allZero(a) {
			s.free(first)
			o.indirect = 0
		}
	}
	if size <= 8*blockSize {
		for i := (size + blockSize - 1) / blockSize; i < 8; i++ {
			s.free(o.direct[i])
			o.direct[i] = 0
		}
	}
	o.size = size
	return nil
}

// freeContent 在对象或存储档位撤下内容时登记释放，读取映射但不读取数据块。
func (s *blockStore) freeContent(o *object) error {
	if o.size <= 512 {
		return nil
	}
	if o.size > 8*blockSize {
		return s.freeIndirect(o.indirect, 0)
	}
	for _, id := range o.direct {
		s.free(id)
	}
	return nil
}

// freeIndirect 撤下映射页，转成直接块时保留已经转移给对象的前几个数据块。
func (s *blockStore) freeIndirect(root, keep uint32) error {
	if root == 0 {
		return nil
	}
	a, err := s.read(root)
	if err != nil {
		return err
	}
	for i := uint32(0); i < 1024; i++ {
		id := get32(a, int(i)*4)
		start := uint32(0)
		if keep > i*1024 {
			start = min(1024, keep-i*1024)
		}
		if err := s.freeSecond(id, start); err != nil {
			return err
		}
	}
	s.free(root)
	return nil
}

func (s *blockStore) freeSecond(id, start uint32) error {
	if id == 0 {
		return nil
	}
	b, err := s.read(id)
	if err != nil {
		return err
	}
	for i := start; i < 1024; i++ {
		s.free(get32(b, int(i)*4))
	}
	s.free(id)
	return nil
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func (s *blockStore) writeContent(o *object, src []byte, off int64) (int, error) {
	if len(src) == 0 {
		return 0, nil
	}
	end := uint32(off + int64(len(src)))
	if end > o.size {
		if err := s.resizeContent(o, end); err != nil {
			return 0, err
		}
	}
	if o.size <= 512 {
		return copy(o.inline[int(off):], src), nil
	}
	n := 0
	for n < len(src) {
		pos := off + int64(n)
		index := uint32(pos / blockSize)
		start := int(pos % blockSize)
		count := min(len(src)-n, blockSize-start)
		id, err := s.dataID(o, index)
		if err != nil {
			return n, err
		}
		var next uint32
		var b []byte
		_, owned := s.owned[id]
		if start == 0 && count == blockSize && !owned {
			next, err = s.alloc(nil)
			b = s.dirty[next]
			if err == nil {
				s.free(id)
			}
		} else {
			next, b, err = s.cow(id)
		}
		if err != nil {
			return n, err
		}
		copy(b[start:start+count], src[n:n+count])
		if err := s.setDataID(o, index, next); err != nil {
			return n, err
		}
		n += count
		if err := s.spill(); err != nil {
			return n, err
		}
	}
	return n, nil
}
