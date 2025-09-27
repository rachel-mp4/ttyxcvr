package main

import ()

type EditType = int

const (
	EditAdd EditType = iota
	EditKeep
	EditDel
	EditNil
)

type Edit struct {
	EditType  EditType
	Utf16Text []uint16
}

type Editstring struct {
	EditType EditType
	Text     string
}

type EditSegment struct {
	weight int
	aidx   int
	bidx   int
	parent *EditSegment
}

type coordinate struct {
	a int
	b int
}

type SegmentHeap struct {
	segments []*EditSegment
	searched map[coordinate]bool
}

func NewSegmentHeap() SegmentHeap {
	segments := make([]*EditSegment, 0, 10)
	searched := make(map[coordinate]bool)
	return SegmentHeap{segments, searched}
}

func (h *SegmentHeap) Add(seg *EditSegment) {
	searched := h.searched[coordinate{seg.aidx, seg.bidx}]
	if searched {
		return
	}
	h.segments = append(h.segments, seg)
	h.searched[coordinate{seg.aidx, seg.bidx}] = true
	h.siftUp(len(h.segments) - 1)
}

func (h *SegmentHeap) PopFront() *EditSegment {
	if len(h.segments) == 0 {
		return nil
	}
	front := h.segments[0]
	if len(h.segments) == 1 {
		h.segments = nil
		return front
	}
	h.segments[0] = h.segments[len(h.segments)-1]
	h.segments = h.segments[:len(h.segments)-1]
	h.siftDown(0)
	return front
}

func (h *SegmentHeap) siftUp(idx int) {
	if idx == 0 {
		return
	}
	loweridx := idx
	upperidx := (idx - 1) / 2
	lower := h.segments[loweridx]
	upper := h.segments[upperidx]
	if lower.lighter(upper) {
		h.segments[upperidx] = lower
		h.segments[loweridx] = upper
		h.siftUp(upperidx)
	}
}

func (h *SegmentHeap) siftDown(idx int) {
	upperidx := idx
	var swap *EditSegment
	loweridx := idx*2 + 1
	lower2idx := idx*2 + 2
	if loweridx < len(h.segments) && h.segments[loweridx].lighter(h.segments[upperidx]) {
		swap = h.segments[upperidx]
		h.segments[upperidx] = h.segments[loweridx]
		h.segments[loweridx] = swap
		h.siftDown(loweridx)
		return
	}
	if lower2idx < len(h.segments) && h.segments[lower2idx].lighter(h.segments[upperidx]) {
		swap = h.segments[upperidx]
		h.segments[upperidx] = h.segments[lower2idx]
		h.segments[lower2idx] = swap
		h.siftDown(lower2idx)
		return
	}

}

func (seg *EditSegment) lighter(A *EditSegment) bool {
	if seg.weight < A.weight {
		return true
	} else if seg.weight > A.weight {
		return false
	} else {
		return seg.aidx+seg.bidx > A.aidx+A.bidx
	}
}

// Diff calculates the diff between wordA and wordB as a miniaml slice of
// edits that you have to make to wordA so that you end up with wordB
func Diff(wordA []uint16, wordB []uint16) []Edit {
	heap := NewSegmentHeap()
	head := EditSegment{0, 0, 0, nil}
	heap.Add(&head)
	segment := heap.PopFront()
	for !(segment.aidx == len(wordA) && segment.bidx == len(wordB)) {
		if segment.aidx != len(wordA) &&
			segment.bidx != len(wordB) &&
			wordA[segment.aidx] == wordB[segment.bidx] {
			newSegment := EditSegment{segment.weight, segment.aidx + 1, segment.bidx + 1, segment}
			heap.Add(&newSegment)
		}
		if segment.aidx != len(wordA) {
			newSegment := EditSegment{segment.weight + 1, segment.aidx + 1, segment.bidx, segment}
			heap.Add(&newSegment)
		}
		if segment.bidx != len(wordB) {
			newSegment := EditSegment{segment.weight + 1, segment.aidx, segment.bidx + 1, segment}
			heap.Add(&newSegment)
		}
		segment = heap.PopFront()
	}
	prevSegment := segment.parent
	edits := make([]Edit, 0)
	currentEdit := Edit{EditNil, nil}
	for prevSegment != nil {
		diffA := prevSegment.aidx != segment.aidx
		diffB := prevSegment.bidx != segment.bidx
		var et EditType
		var char uint16
		if diffA && diffB {
			et = EditKeep
			char = wordA[prevSegment.aidx]
		} else if diffA {
			et = EditDel
			char = wordA[prevSegment.aidx]
		} else if diffB {
			et = EditAdd
			char = wordB[prevSegment.bidx]
		} else {
			et = EditNil
		}
		if currentEdit.EditType != et {
			if currentEdit.EditType != EditNil {
				edits = append([]Edit{currentEdit}, edits...)
			}
			currentEdit = Edit{et, []uint16{char}}
		} else {
			currentEdit.Utf16Text = append([]uint16{char}, currentEdit.Utf16Text...)
		}
		segment = prevSegment
		prevSegment = segment.parent
	}
	edits = append([]Edit{currentEdit}, edits...)
	return edits
}

func Diffs(wordA string, wordB string) []Editstring {
	heap := NewSegmentHeap()
	head := EditSegment{0, 0, 0, nil}
	heap.Add(&head)
	segment := heap.PopFront()
	for !(segment.aidx == len(wordA) && segment.bidx == len(wordB)) {
		if segment.aidx != len(wordA) &&
			segment.bidx != len(wordB) &&
			wordA[segment.aidx] == wordB[segment.bidx] {
			newSegment := EditSegment{segment.weight, segment.aidx + 1, segment.bidx + 1, segment}
			heap.Add(&newSegment)
		}
		if segment.aidx != len(wordA) {
			newSegment := EditSegment{segment.weight + 1, segment.aidx + 1, segment.bidx, segment}
			heap.Add(&newSegment)
		}
		if segment.bidx != len(wordB) {
			newSegment := EditSegment{segment.weight + 1, segment.aidx, segment.bidx + 1, segment}
			heap.Add(&newSegment)
		}
		segment = heap.PopFront()
	}
	prevSegment := segment.parent
	edits := make([]Editstring, 0)
	currentEdit := Editstring{EditNil, ""}
	for prevSegment != nil {
		diffA := prevSegment.aidx != segment.aidx
		diffB := prevSegment.bidx != segment.bidx
		var et EditType
		var char string
		if diffA && diffB {
			et = EditKeep
			char = string(wordA[prevSegment.aidx])
		} else if diffA {
			et = EditDel
			char = string(wordA[prevSegment.aidx])
		} else if diffB {
			et = EditAdd
			char = string(wordB[prevSegment.bidx])
		} else {
			et = EditNil
		}
		if currentEdit.EditType != et {
			if currentEdit.EditType != EditNil {
				edits = append([]Editstring{currentEdit}, edits...)
			}
			currentEdit = Editstring{et, char}
		} else {
			currentEdit.Text = char + currentEdit.Text
		}
		segment = prevSegment
		prevSegment = segment.parent
	}
	edits = append([]Editstring{currentEdit}, edits...)
	return edits
}
