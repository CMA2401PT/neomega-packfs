package packfs

import (
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
	"os"
	"sort"
)

const blockSize = 4096
const dirtyLimit = 64 << 20

// blockIO 把持久化边界缩到四个操作，便于模拟掉电时未同步的写入。
type blockIO interface {
	ReadAt([]byte, int64) (int, error)
	WriteAt([]byte, int64) (int, error)
	Truncate(int64) error
	Sync() error
}

// blockStore 的位图在发布前同时保留已提交版本和事务分配。
// 所有权独立于脏缓冲，快照边界后的分配才能在普通提交中直接复用。
// 事务结束后换用新集合，避免大事务留下的大表拖慢小提交。
type blockStore struct {
	io           blockIO
	used         []uint64
	usedCount    uint64
	dirty        map[uint32][]byte
	owned        map[uint32]struct{}
	young        map[uint32]struct{}
	freed        map[uint32]struct{}
	oldGarbage   uint64
	hint         uint32
	end          uint64
	diskSize     int64
	onWriteError func(error) error
}

func newStore(f blockIO, size int64) *blockStore {
	return &blockStore{
		io:       f,
		dirty:    make(map[uint32][]byte),
		owned:    make(map[uint32]struct{}),
		young:    make(map[uint32]struct{}),
		freed:    make(map[uint32]struct{}),
		hint:     2,
		end:      uint64((size + blockSize - 1) / blockSize),
		diskSize: size,
	}
}

func (s *blockStore) has(id uint32) bool {
	return int(id/64) < len(s.used) && s.used[id/64]&(uint64(1)<<(id%64)) != 0
}

func (s *blockStore) mark(id uint32) {
	for int(id/64) >= len(s.used) {
		s.used = append(s.used, 0)
	}
	mask := uint64(1) << (id % 64)
	if s.used[id/64]&mask == 0 {
		s.used[id/64] |= mask
		s.usedCount++
	}
}

func (s *blockStore) alloc(data []byte) (uint32, error) {
	id := s.hint
	for s.has(id) {
		if id == ^uint32(0) {
			return 0, fmt.Errorf("packfs: block address space exhausted")
		}
		id++
	}
	s.mark(id)
	s.hint = id
	if uint64(id) >= s.end {
		s.end = uint64(id) + 1
	}
	b := make([]byte, blockSize)
	copy(b, data)
	s.dirty[id] = b
	s.owned[id] = struct{}{}
	s.young[id] = struct{}{}
	return id, nil
}

func (s *blockStore) read(id uint32) ([]byte, error) {
	if b, ok := s.dirty[id]; ok {
		return b, nil
	}
	b := make([]byte, blockSize)
	_, err := s.io.ReadAt(b, int64(id)*blockSize)
	return b, err
}

// cow 依据事务所有权复用块，提前写出的本事务块需要先读回缓冲。
func (s *blockStore) cow(id uint32) (uint32, []byte, error) {
	if _, ok := s.owned[id]; ok {
		b, err := s.read(id)
		if err != nil {
			return 0, nil, err
		}
		s.dirty[id] = b
		return id, b, nil
	}
	var b []byte
	var err error
	if id != 0 {
		b, err = s.read(id)
		if err != nil {
			return 0, nil, err
		}
	}
	n, err := s.alloc(b)
	if err != nil {
		return 0, nil, err
	}
	s.free(id)
	return n, s.dirty[n], nil
}

// free 只登记撤下的引用，提交发布前不能让分配器复用旧块。
func (s *blockStore) free(id uint32) {
	if id != 0 {
		s.freed[id] = struct{}{}
	}
}

// reclaim 只立即回收快照边界之后的块，其余候选由全量扫描判定是否仍被共享。
func (s *blockStore) reclaim() {
	for id := range s.freed {
		if _, ok := s.young[id]; ok {
			s.used[id/64] &^= uint64(1) << (id % 64)
			s.usedCount--
			delete(s.young, id)
			if id < s.hint {
				s.hint = id
			}
		} else {
			s.oldGarbage++
		}
	}
	s.freed = make(map[uint32]struct{})
	word := (s.end - 1) / 64
	for s.used[word] == 0 {
		word--
	}
	s.end = word*64 + uint64(bits.Len64(s.used[word]))
}

// spill 在完整更新之间写出缓冲，调用者此时不再持有需要继续修改的页面。
func (s *blockStore) spill() error {
	if len(s.dirty) > dirtyLimit/blockSize {
		return s.flush()
	}
	return nil
}

// flush 不发布任何根，已写出的块仍属于当前事务，可以再次读回修改。
func (s *blockStore) flush() error {
	ids := make([]uint32, 0, len(s.dirty))
	for id := range s.dirty {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if err := writeFullAt(s.io, s.dirty[id], int64(id)*blockSize); err != nil {
			return s.onWriteError(err)
		}
		s.diskSize = max(s.diskSize, (int64(id)+1)*blockSize)
		delete(s.dirty, id)
	}
	s.dirty = make(map[uint32][]byte)
	return nil
}

func writeFullAt(f blockIO, b []byte, off int64) error {
	n, err := f.WriteAt(b, off)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return err
}

func get32(b []byte, i int) uint32    { return binary.LittleEndian.Uint32(b[i : i+4]) }
func put32(b []byte, i int, v uint32) { binary.LittleEndian.PutUint32(b[i:i+4], v) }

// syncDirectory 让替换容器文件的目录项也进入持久化边界。
func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
