/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2021 WireGuard LLC. All Rights Reserved.
 */

package device

import (
	"container/list"
	"errors"
	"math/bits"
	"net"
	"sync"
	"unsafe"
)

type parentIndirection struct {
	parentBit     **trieEntry
	parentBitType uint8
}

type trieEntry struct {
	peer        *Peer
	child       [2]*trieEntry
	parent      parentIndirection
	cidr        uint8
	bitAtByte   uint8
	bitAtShift  uint8
	bits        net.IP
	perPeerElem *list.Element
}

func isLittleEndian() bool {
	one := uint32(1)
	return *(*byte)(unsafe.Pointer(&one)) != 0
}

func swapU32(i uint32) uint32 {
	if !isLittleEndian() {
		return i
	}

	return bits.ReverseBytes32(i)
}

func swapU64(i uint64) uint64 {
	if !isLittleEndian() {
		return i
	}

	return bits.ReverseBytes64(i)
}

func commonBits(ip1 net.IP, ip2 net.IP) uint8 {
	size := len(ip1)
	if size == net.IPv4len {
		a := (*uint32)(unsafe.Pointer(&ip1[0]))
		b := (*uint32)(unsafe.Pointer(&ip2[0]))
		x := *a ^ *b
		return uint8(bits.LeadingZeros32(swapU32(x)))
	} else if size == net.IPv6len {
		a := (*uint64)(unsafe.Pointer(&ip1[0]))
		b := (*uint64)(unsafe.Pointer(&ip2[0]))
		x := *a ^ *b
		if x != 0 {
			return uint8(bits.LeadingZeros64(swapU64(x)))
		}
		a = (*uint64)(unsafe.Pointer(&ip1[8]))
		b = (*uint64)(unsafe.Pointer(&ip2[8]))
		x = *a ^ *b
		return 64 + uint8(bits.LeadingZeros64(swapU64(x)))
	} else {
		panic("Wrong size bit string")
	}
}

func (node *trieEntry) addToPeerEntries() {
	node.perPeerElem = node.peer.trieEntries.PushBack(node)
}

func (node *trieEntry) removeFromPeerEntries() {
	if node.perPeerElem != nil {
		node.peer.trieEntries.Remove(node.perPeerElem)
		node.perPeerElem = nil
	}
}

func (node *trieEntry) removeByPeer(p *Peer) *trieEntry {
	if node == nil {
		return node
	}

	// walk recursively

	node.child[0] = node.child[0].removeByPeer(p)
	node.child[1] = node.child[1].removeByPeer(p)

	if node.peer != p {
		return node
	}

	// remove peer & merge

	node.removeFromPeerEntries()
	node.peer = nil
	if node.child[0] == nil {
		return node.child[1]
	}
	return node.child[0]
}

func (node *trieEntry) choose(ip net.IP) byte {
	return (ip[node.bitAtByte] >> node.bitAtShift) & 1
}

func (node *trieEntry) maskSelf() {
	mask := net.CIDRMask(int(node.cidr), len(node.bits)*8)
	for i := 0; i < len(mask); i++ {
		node.bits[i] &= mask[i]
	}
}

func (node *trieEntry) nodePlacement(ip net.IP, cidr uint8) (parent *trieEntry, exact bool) {
	for node != nil && node.cidr <= cidr && commonBits(node.bits, ip) >= node.cidr {
		parent = node
		if parent.cidr == cidr {
			exact = true
			return
		}
		bit := node.choose(ip)
		node = node.child[bit]
	}
	return
}

func (trie parentIndirection) insert(ip net.IP, cidr uint8, peer *Peer) {
	if *trie.parentBit == nil {
		node := &trieEntry{
			peer:       peer,
			parent:     trie,
			bits:       ip,
			cidr:       cidr,
			bitAtByte:  cidr / 8,
			bitAtShift: 7 - (cidr % 8),
		}
		node.maskSelf()
		node.addToPeerEntries()
		*trie.parentBit = node
		return
	}
	node, exact := (*trie.parentBit).nodePlacement(ip, cidr)
	if exact {
		node.removeFromPeerEntries()
		node.peer = peer
		node.addToPeerEntries()
		return
	}

	newNode := &trieEntry{
		peer:       peer,
		bits:       ip,
		cidr:       cidr,
		bitAtByte:  cidr / 8,
		bitAtShift: 7 - (cidr % 8),
	}
	newNode.maskSelf()
	newNode.addToPeerEntries()

	var down *trieEntry
	if node == nil {
		down = *trie.parentBit
	} else {
		bit := node.choose(ip)
		down = node.child[bit]
		if down == nil {
			newNode.parent = parentIndirection{&node.child[bit], bit}
			node.child[bit] = newNode
			return
		}
	}
	common := commonBits(down.bits, ip)
	if common < cidr {
		cidr = common
	}
	parent := node

	if newNode.cidr == cidr {
		bit := newNode.choose(down.bits)
		down.parent = parentIndirection{&newNode.child[bit], bit}
		newNode.child[bit] = down
		if parent == nil {
			newNode.parent = trie
			*trie.parentBit = newNode
		} else {
			bit := parent.choose(newNode.bits)
			newNode.parent = parentIndirection{&parent.child[bit], bit}
			parent.child[bit] = newNode
		}
		return
	}

	node = &trieEntry{
		bits:       append([]byte{}, newNode.bits...),
		cidr:       cidr,
		bitAtByte:  cidr / 8,
		bitAtShift: 7 - (cidr % 8),
	}
	node.maskSelf()

	bit := node.choose(down.bits)
	down.parent = parentIndirection{&node.child[bit], bit}
	node.child[bit] = down
	bit = node.choose(newNode.bits)
	newNode.parent = parentIndirection{&node.child[bit], bit}
	node.child[bit] = newNode
	if parent == nil {
		node.parent = trie
		*trie.parentBit = node
	} else {
		bit := parent.choose(node.bits)
		node.parent = parentIndirection{&parent.child[bit], bit}
		parent.child[bit] = node
	}
}

func (node *trieEntry) lookup(ip net.IP) *Peer {
	var found *Peer
	size := uint8(len(ip))
	for node != nil && commonBits(node.bits, ip) >= node.cidr {
		if node.peer != nil {
			found = node.peer
		}
		if node.bitAtByte == size {
			break
		}
		bit := node.choose(ip)
		node = node.child[bit]
	}
	return found
}

type AllowedIPs struct {
	IPv4  *trieEntry
	IPv6  *trieEntry
	mutex sync.RWMutex
}

func (table *AllowedIPs) EntriesForPeer(peer *Peer, cb func(ip net.IP, cidr uint8) bool) {
	table.mutex.RLock()
	defer table.mutex.RUnlock()

	for elem := peer.trieEntries.Front(); elem != nil; elem = elem.Next() {
		node := elem.Value.(*trieEntry)
		if !cb(node.bits, node.cidr) {
			return
		}
	}
}

func (table *AllowedIPs) RemoveByPeer(peer *Peer) {
	table.mutex.Lock()
	defer table.mutex.Unlock()

	table.IPv4 = table.IPv4.removeByPeer(peer)
	table.IPv6 = table.IPv6.removeByPeer(peer)
}

func (table *AllowedIPs) Insert(ip net.IP, cidr uint8, peer *Peer) {
	table.mutex.Lock()
	defer table.mutex.Unlock()

	switch len(ip) {
	case net.IPv6len:
		parentIndirection{&table.IPv6, 2}.insert(ip, cidr, peer)
	case net.IPv4len:
		parentIndirection{&table.IPv4, 2}.insert(ip, cidr, peer)
	default:
		panic(errors.New("inserting unknown address type"))
	}
}

func (table *AllowedIPs) LookupIPv4(address []byte) *Peer {
	table.mutex.RLock()
	defer table.mutex.RUnlock()
	return table.IPv4.lookup(address)
}

func (table *AllowedIPs) LookupIPv6(address []byte) *Peer {
	table.mutex.RLock()
	defer table.mutex.RUnlock()
	return table.IPv6.lookup(address)
}
