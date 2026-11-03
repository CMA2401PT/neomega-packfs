package packfs

import (
	"bytes"
	"encoding/binary"
	"sort"
)

// treeNode 的内节点保存各子树的最小键；叶子不保存跨版本失效的相邻页指针。
type treeNode struct {
	leaf     bool
	keys     [][]byte
	values   [][]byte
	children []uint32
}

func decodeNode(b []byte) *treeNode {
	n := &treeNode{leaf: b[0] == 1}
	count := int(binary.LittleEndian.Uint16(b[1:3]))
	off := 3
	for i := 0; i < count; i++ {
		kl := int(binary.LittleEndian.Uint16(b[off : off+2]))
		off += 2
		var vl int
		if n.leaf {
			vl = int(binary.LittleEndian.Uint16(b[off : off+2]))
			off += 2
		}
		n.keys = append(n.keys, bytes.Clone(b[off:off+kl]))
		off += kl
		if n.leaf {
			n.values = append(n.values, bytes.Clone(b[off:off+vl]))
			off += vl
		} else {
			n.children = append(n.children, get32(b, off))
			off += 4
		}
	}
	return n
}

func (n *treeNode) size() int {
	s := 3
	for i, k := range n.keys {
		s += 2 + len(k)
		if n.leaf {
			s += 2 + len(n.values[i])
		} else {
			s += 4
		}
	}
	return s
}

func (n *treeNode) encode() []byte {
	b := make([]byte, blockSize)
	if n.leaf {
		b[0] = 1
	} else {
		b[0] = 2
	}
	binary.LittleEndian.PutUint16(b[1:3], uint16(len(n.keys)))
	off := 3
	for i, k := range n.keys {
		binary.LittleEndian.PutUint16(b[off:off+2], uint16(len(k)))
		off += 2
		if n.leaf {
			binary.LittleEndian.PutUint16(b[off:off+2], uint16(len(n.values[i])))
			off += 2
		}
		copy(b[off:], k)
		off += len(k)
		if n.leaf {
			copy(b[off:], n.values[i])
			off += len(n.values[i])
		} else {
			put32(b, off, n.children[i])
			off += 4
		}
	}
	return b
}

func (s *blockStore) node(id uint32) (*treeNode, error) {
	b, err := s.read(id)
	if err != nil {
		return nil, err
	}
	return decodeNode(b), nil
}

func childIndex(n *treeNode, key []byte) int {
	i := sort.Search(len(n.keys), func(i int) bool { return bytes.Compare(n.keys[i], key) > 0 }) - 1
	if i < 0 {
		return 0
	}
	return i
}

func (s *blockStore) treeGet(root uint32, key []byte) ([]byte, error) {
	for root != 0 {
		n, err := s.node(root)
		if err != nil {
			return nil, err
		}
		if n.leaf {
			i := sort.Search(len(n.keys), func(i int) bool { return bytes.Compare(n.keys[i], key) >= 0 })
			if i < len(n.keys) && bytes.Equal(n.keys[i], key) {
				return n.values[i], nil
			}
			return nil, nil
		}
		root = n.children[childIndex(n, key)]
	}
	return nil, nil
}

type treeRef struct {
	key []byte
	id  uint32
}

func (s *blockStore) saveNode(id uint32, n *treeNode) (treeRef, error) {
	if id == 0 {
		next, err := s.alloc(n.encode())
		return treeRef{n.keys[0], next}, err
	}
	next, b, err := s.cow(id)
	if err != nil {
		return treeRef{}, err
	}
	copy(b, n.encode())
	return treeRef{n.keys[0], next}, nil
}

// treeEdit 自底向上复制搜索路径，分裂或合并的影响只沿父节点传播。
func (s *blockStore) treeEdit(id uint32, key, value []byte) ([]treeRef, error) {
	n := &treeNode{leaf: true}
	if id != 0 {
		var err error
		n, err = s.node(id)
		if err != nil {
			return nil, err
		}
	}
	if n.leaf {
		i := sort.Search(len(n.keys), func(i int) bool { return bytes.Compare(n.keys[i], key) >= 0 })
		found := i < len(n.keys) && bytes.Equal(n.keys[i], key)
		if value == nil {
			if !found {
				if id == 0 {
					return nil, nil
				}
				return []treeRef{{n.keys[0], id}}, nil
			}
			n.keys = append(n.keys[:i], n.keys[i+1:]...)
			n.values = append(n.values[:i], n.values[i+1:]...)
		} else if found {
			n.values[i] = bytes.Clone(value)
		} else {
			n.keys = append(n.keys, nil)
			copy(n.keys[i+1:], n.keys[i:])
			n.keys[i] = bytes.Clone(key)
			n.values = append(n.values, nil)
			copy(n.values[i+1:], n.values[i:])
			n.values[i] = bytes.Clone(value)
		}
	} else {
		i := childIndex(n, key)
		refs, err := s.treeEdit(n.children[i], key, value)
		if err != nil {
			return nil, err
		}
		keys := append([][]byte(nil), n.keys[:i]...)
		children := append([]uint32(nil), n.children[:i]...)
		for _, r := range refs {
			keys = append(keys, r.key)
			children = append(children, r.id)
		}
		n.keys = append(keys, n.keys[i+1:]...)
		n.children = append(children, n.children[i+1:]...)
		if value == nil {
			if err := s.mergeChildren(n); err != nil {
				return nil, err
			}
		}
	}
	if len(n.keys) == 0 {
		s.free(id)
		return nil, nil
	}
	if n.size() <= blockSize {
		r, err := s.saveNode(id, n)
		return []treeRef{r}, err
	}
	cut := n.splitPoint()
	a, err := s.saveNode(id, n.slice(0, cut))
	if err != nil {
		return nil, err
	}
	b, err := s.saveNode(0, n.slice(cut, len(n.keys)))
	if err != nil {
		return nil, err
	}
	return []treeRef{a, b}, nil
}

// mergeChildren 在删除后合并能容纳于一页的相邻子树，避免长期留下碎页。
func (s *blockStore) mergeChildren(n *treeNode) error {
	for j := 0; j+1 < len(n.children); {
		a, err := s.node(n.children[j])
		if err != nil {
			return err
		}
		b, err := s.node(n.children[j+1])
		if err != nil {
			return err
		}
		if a.size()+b.size()-3 > blockSize {
			j++
			continue
		}
		a.keys = append(a.keys, b.keys...)
		a.values = append(a.values, b.values...)
		a.children = append(a.children, b.children...)
		r, err := s.saveNode(n.children[j], a)
		if err != nil {
			return err
		}
		n.keys[j] = r.key
		s.free(n.children[j+1])
		n.children[j] = r.id
		n.keys = append(n.keys[:j+1], n.keys[j+2:]...)
		n.children = append(n.children[:j+1], n.children[j+2:]...)
	}
	return nil
}

// splitPoint 按字节数选分裂点，避免大内联记录使一侧超出一页。
func (n *treeNode) splitPoint() int {
	cut, best := 1, blockSize*2
	for i := 1; i < len(n.keys); i++ {
		a, b := n.slice(0, i), n.slice(i, len(n.keys))
		diff := a.size() - b.size()
		if diff < 0 {
			diff = -diff
		}
		if a.size() <= blockSize && b.size() <= blockSize && diff < best {
			cut, best = i, diff
		}
	}
	return cut
}

func (n *treeNode) slice(lo, hi int) *treeNode {
	r := &treeNode{leaf: n.leaf, keys: n.keys[lo:hi]}
	if n.leaf {
		r.values = n.values[lo:hi]
	} else {
		r.children = n.children[lo:hi]
	}
	return r
}

func (s *blockStore) treeSet(root uint32, key, value []byte) (uint32, error) {
	refs, err := s.treeEdit(root, key, value)
	if err != nil {
		return 0, s.onWriteError(err)
	}
	if len(refs) == 0 {
		return 0, s.spill()
	}
	if len(refs) == 1 {
		root = refs[0].id
		for {
			n, err := s.node(root)
			if err != nil {
				return 0, s.onWriteError(err)
			}
			if n.leaf || len(n.children) != 1 {
				return root, s.spill()
			}
			s.free(root)
			root = n.children[0]
		}
	}
	n := &treeNode{}
	for _, r := range refs {
		n.keys = append(n.keys, r.key)
		n.children = append(n.children, r.id)
	}
	r, err := s.saveNode(0, n)
	if err != nil {
		return 0, s.onWriteError(err)
	}
	return r.id, s.spill()
}

// treeRange 的递归调用栈就是父节点栈，范围遍历始终从同一个根出发。
func (s *blockStore) treeRange(root uint32, prefix []byte, visit func([]byte, []byte) error) error {
	if root == 0 {
		return nil
	}
	n, err := s.node(root)
	if err != nil {
		return err
	}
	if n.leaf {
		for i, k := range n.keys {
			if bytes.HasPrefix(k, prefix) {
				if err := visit(k, n.values[i]); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for i, child := range n.children {
		if i+1 < len(n.keys) && bytes.Compare(n.keys[i+1], prefix) <= 0 {
			continue
		}
		if bytes.Compare(n.keys[i], prefix) > 0 && !bytes.HasPrefix(n.keys[i], prefix) {
			break
		}
		if err := s.treeRange(child, prefix, visit); err != nil {
			return err
		}
	}
	return nil
}
