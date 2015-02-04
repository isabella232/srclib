package store

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"sort"
	"strings"

	"github.com/alecthomas/binary"
	"github.com/smartystreets/mafsa"

	"sourcegraph.com/sourcegraph/srclib/unit"
)

type defQueryTreeIndex struct {
	mt    *mafsaUnitTable
	ready bool
}

var _ interface {
	Index
	persistedIndex
	defQueryTreeIndexBuilder
	defTreeIndex
} = (*defQueryTreeIndex)(nil)

var c_defQueryTreeIndex_getByQuery = 0 // counter

func (x *defQueryTreeIndex) String() string {
	return fmt.Sprintf("defQueryTreeIndex(ready=%v)", x.ready)
}

func (x *defQueryTreeIndex) getByQuery(q string) (map[unit.ID2]byteOffsets, bool) {
	vlog.Printf("defQueryTreeIndex.getByQuery(%q)", q)
	c_defQueryTreeIndex_getByQuery++

	if x.mt == nil {
		panic("mafsaTable not built/read")
	}

	q = strings.ToLower(q)
	node, i := x.mt.t.IndexedTraverse([]rune(q))
	if node == nil {
		return nil, false
	}
	nn := node.Number
	if node.Final {
		i--
		nn++
	}
	uofMap := map[unit.ID2]byteOffsets{}
	numDefs := 0
	for _, unitsOffsets := range x.mt.Values[i : i+nn] {
		for _, uofs := range unitsOffsets {
			u := x.mt.Units[uofs.Unit]
			uofMap[u] = append(uofMap[u], deltaDecode(uofs.byteOffsets)...)
			numDefs += len(uofs.byteOffsets)
		}
	}
	vlog.Printf("defQueryTreeIndex.getByQuery(%q): found %d defs.", q, numDefs)
	return uofMap, true
}

// Covers implements defIndex.
func (x *defQueryTreeIndex) Covers(filters interface{}) int {
	cov := 0
	for _, f := range storeFilters(filters) {
		if _, ok := f.(ByDefQueryFilter); ok {
			cov++
		}
	}
	return cov
}

// Defs implements defIndex.
func (x *defQueryTreeIndex) Defs(f ...DefFilter) (map[unit.ID2]byteOffsets, error) {
	for _, ff := range f {
		if pf, ok := ff.(ByDefQueryFilter); ok {
			uofmap, found := x.getByQuery(pf.ByDefQuery())
			if !found {
				return nil, nil
			}
			return uofmap, nil
		}
	}
	return nil, nil
}

// Build implements defQueryTreeIndexBuilder.
func (x *defQueryTreeIndex) Build(xs map[unit.ID2]*defQueryIndex) (err error) {
	vlog.Printf("defQueryTreeIndex: building index... (%d unit indexes)", len(xs))

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in defQueryTreeIndex.Build (%d unit indexes): %v", len(xs), err)
		}
	}()

	units := make([]unit.ID2, 0, len(xs))
	for u := range xs {
		units = append(units, u)
	}
	sort.Sort(unitID2s(units))

	const maxUnits = math.MaxUint8
	if len(units) > maxUnits {
		log.Printf("Warning: the def query index supports a maximum of %d source units in a tree, but this tree has %d. Source units that exceed the limit will not be indexed for def queries.", maxUnits, len(units))
		units = units[:maxUnits]
	}

	unitNums := make(map[unit.ID2]uint8, len(units))
	for _, u := range units {
		unitNums[u] = uint8(len(unitNums))
	}

	termToUOffs := make(map[string][]unitOffsets)

	var traverse func(term string, unit uint8, node *mafsa.MinTreeNode)
	for u, qx := range xs {
		i := 0
		traverse = func(term string, unit uint8, node *mafsa.MinTreeNode) {
			if node == nil {
				return
			}
			if node.Final {
				uoffs := unitOffsets{Unit: unit, byteOffsets: deltaEncode(qx.mt.Values[i])}
				termToUOffs[term] = append(termToUOffs[term], uoffs)
				i++
			}
			for _, e := range node.OrderedEdges() {
				traverse(term+string([]rune{e}), unit, node.Edges[e])
			}
		}
		if qx.mt.t != nil {
			traverse("", unitNums[u], qx.mt.t.Root)
		}
	}
	vlog.Printf("defQueryTreeIndex: done traversing unit indexes.")

	terms := make([]string, 0, len(termToUOffs))
	for term := range termToUOffs {
		terms = append(terms, term)
	}
	sort.Strings(terms)

	if len(terms) == 0 {
		x.mt = &mafsaUnitTable{}
		x.ready = true
		return nil
	}

	bt := mafsa.New()
	x.mt = &mafsaUnitTable{}
	x.mt.Values = make([][]unitOffsets, len(terms))
	for i, term := range terms {
		bt.Insert(term)
		x.mt.Values[i] = termToUOffs[term]
	}
	bt.Finish()
	vlog.Printf("defQueryTreeIndex: done adding %d terms to MAFSA & table and minimizing.", len(terms))

	b, err := bt.MarshalBinary()
	if err != nil {
		return err
	}
	vlog.Printf("defQueryTreeIndex: done serializing MAFSA & table to %d bytes.", len(b))

	x.mt.B = b
	x.mt.Units = units
	x.mt.t, err = new(mafsa.Decoder).Decode(x.mt.B)
	if err != nil {
		return err
	}
	x.ready = true
	vlog.Printf("defQueryTreeIndex: done building index (%d terms).", len(terms))
	return nil
}

// Write implements persistedIndex.
func (x *defQueryTreeIndex) Write(w io.Writer) error {
	if x.mt == nil {
		panic("no mafsaTable to write")
	}
	b, err := binary.Marshal(x.mt)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// Read implements persistedIndex.
func (x *defQueryTreeIndex) Read(r io.Reader) error {
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return err
	}
	var mt mafsaUnitTable
	err = binary.Unmarshal(b, &mt)
	x.mt = &mt
	if err == nil && len(x.mt.B) > 0 {
		x.mt.t, err = new(mafsa.Decoder).Decode(x.mt.B)
	}
	x.ready = (err == nil)
	return err
}

// Ready implements persistedIndex.
func (x *defQueryTreeIndex) Ready() bool { return x.ready }

type unitID2s []unit.ID2

func (v unitID2s) Len() int      { return len(v) }
func (v unitID2s) Swap(i, j int) { v[i], v[j] = v[j], v[i] }
func (v unitID2s) Less(i, j int) bool {
	return v[i].Name < v[j].Name || (v[i].Name == v[j].Name && v[i].Type < v[j].Type)
}

type int64Slice []int64

func (v int64Slice) Len() int           { return len(v) }
func (v int64Slice) Swap(i, j int)      { v[i], v[j] = v[j], v[i] }
func (v int64Slice) Less(i, j int) bool { return v[i] < v[j] }

// A mafsaUnitTable like a mafsaTable but stores unitOffsets not
// byteOffsets.
type mafsaUnitTable struct {
	t      *mafsa.MinTree
	B      []byte          // bytes of the MinTree
	Units  []unit.ID2      // indexed by their unit index assigned during building the index
	Values [][]unitOffsets // one value per entry in build or min
}
