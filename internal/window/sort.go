package window

import (
	"fmt"
	"sort"

	"dragonboat/windowengine/internal/table"
	"dragonboat/windowengine/internal/value"
)

// sortKeyPlan is a compiled sort key.
type sortKeyPlan struct {
	key  SortKey
	kind value.Kind
}

// orderSignature canonicalises an ordering so that identical orderings (same
// expressions, directions and null placement) share one sort.
func orderSignature(order []SortKey) string {
	if order == nil {
		return ""
	}
	var b []byte
	for _, k := range order {
		dir := byte('a')
		if k.Desc {
			dir = 'd'
		}
		nf := byte('l')
		if k.NullsFirst {
			nf = 'f'
		}
		b = append(b, k.Expr.String()...)
		b = append(b, 0, dir, nf, 1)
	}
	return string(b)
}

func partitionSignature(spec *Spec) string {
	var b []byte
	for _, e := range spec.Partition {
		b = append(b, e.String()...)
		b = append(b, 0)
	}
	return string(b)
}

// compareSort compares two precomputed key vectors with PostgreSQL NULL
// ordering rules.
func compareSort(a, b []value.Value, keys []sortKeyPlan) int {
	for i := range keys {
		av, bv := a[i], b[i]
		an, bn := av.IsNull(), bv.IsNull()
		if an || bn {
			if an && bn {
				continue
			}
			if an {
				if keys[i].key.NullsFirst {
					return -1
				}
				return 1
			}
			if keys[i].key.NullsFirst {
				return 1
			}
			return -1
		}
		c, err := value.Compare(av, bv)
		if err != nil {
			return 0
		}
		if c != 0 {
			if keys[i].key.Desc {
				return -c
			}
			return c
		}
	}
	return 0
}

// preparedTable holds the input rows in partition order, each partition
// contiguous.  Sort-key vectors are stored flat (n*len(keys)) for locality and
// to avoid one allocation per row.
type preparedTable struct {
	order      []int
	keyValues  []value.Value // flat: row i keys at [i*k, (i+1)*k)
	keyIsNull  []bool        // flat NULL flags, parallel to keyValues
	keySorted  []value.Key   // flat compact keys for the comparator
	k          int
	partVals   []value.Value // flat partition-key values per ordered row
	partIsNull []bool
	partSorted []value.Key
	pk         int
	partStarts []int
	partEnds   []int
	keys       []sortKeyPlan
}

// keyAt returns the sort-key vector of ordered position i.
func (pt *preparedTable) keyAt(i int) []value.Value {
	return pt.keyValues[i*pt.k : (i+1)*pt.k]
}

// partAt returns the partition-key vector of ordered position i.
func (pt *preparedTable) partAt(i int) []value.Value {
	if pt.pk == 0 {
		return nil
	}
	return pt.partVals[i*pt.pk : (i+1)*pt.pk]
}

// prepare partitions and sorts the table for the given ordering.  All windows
// that share the same partition expressions and ordering are evaluated against
// the same preparedTable, so the sort happens once.
func prepare(t *table.Table, spec *Spec, keys []sortKeyPlan) (*preparedTable, error) {
	n := len(t.Rows)
	k := len(keys)
	pk := len(spec.Partition)
	pt := &preparedTable{
		order:      make([]int, n),
		keyValues:  make([]value.Value, n*k),
		keyIsNull:  make([]bool, n*k),
		keySorted:  make([]value.Key, n*k),
		partVals:   make([]value.Value, n*pk),
		partIsNull: make([]bool, n*pk),
		partSorted: make([]value.Key, n*pk),
		k:          k,
		pk:         pk,
		keys:       keys,
	}
	// Evaluate keys once per row.
	for i, row := range t.Rows {
		kv := pt.keyValues[i*k : (i+1)*k]
		for j, key := range keys {
			v, err := key.key.Expr.Eval(row)
			if err != nil {
				return nil, err
			}
			kv[j] = v
			if v.IsNull() {
				pt.keyIsNull[i*k+j] = true
			} else {
				pt.keySorted[i*k+j] = value.MakeKey(v)
			}
		}
		pv := pt.partVals[i*pk : (i+1)*pk]
		for j, e := range spec.Partition {
			v, err := e.Eval(row)
			if err != nil {
				return nil, err
			}
			pv[j] = v
			if v.IsNull() {
				pt.partIsNull[i*pk+j] = true
			} else {
				pt.partSorted[i*pk+j] = value.MakeKey(v)
			}
		}
		pt.order[i] = i
	}

	idx := pt.order
	// Sort by partition keys then order keys, breaking ties by original row
	// index so that peer order is the input order (deterministic and matching
	// the reference implementation).  An unstable sort with an explicit index
	// tiebreak is considerably faster than a stable merge sort and gives the
	// same result.
	sort.Slice(idx, func(a, b int) bool {
		ia, ib := idx[a], idx[b]
		if c := pt.comparePart(ia, ib); c != 0 {
			return c < 0
		}
		if c := pt.compareKeys(ia, ib); c != 0 {
			return c < 0
		}
		return ia < ib
	})
	// Reorder the flat key arrays into sorted order.
	newKeys := make([]value.Value, n*k)
	newNull := make([]bool, n*k)
	newSorted := make([]value.Key, n*k)
	newParts := make([]value.Value, n*pk)
	newPartNull := make([]bool, n*pk)
	newPartSorted := make([]value.Key, n*pk)
	for i, id := range idx {
		copy(newKeys[i*k:(i+1)*k], pt.keyValues[id*k:(id+1)*k])
		copy(newNull[i*k:(i+1)*k], pt.keyIsNull[id*k:(id+1)*k])
		copy(newSorted[i*k:(i+1)*k], pt.keySorted[id*k:(id+1)*k])
		copy(newParts[i*pk:(i+1)*pk], pt.partVals[id*pk:(id+1)*pk])
		copy(newPartNull[i*pk:(i+1)*pk], pt.partIsNull[id*pk:(id+1)*pk])
		copy(newPartSorted[i*pk:(i+1)*pk], pt.partSorted[id*pk:(id+1)*pk])
	}
	pt.keyValues = newKeys
	pt.keyIsNull = newNull
	pt.keySorted = newSorted
	pt.partVals = newParts
	pt.partIsNull = newPartNull
	pt.partSorted = newPartSorted

	// Partition boundaries (NULLs equal to each other).
	for i := 0; i < n; i++ {
		if i == 0 || !pt.samePart(i-1, i) {
			pt.partStarts = append(pt.partStarts, i)
			if i > 0 {
				pt.partEnds = append(pt.partEnds, i)
			}
		}
	}
	if n > 0 {
		pt.partEnds = append(pt.partEnds, n)
	}
	return pt, nil
}

// comparePart compares the partition keys of ordered positions a and b using
// the precomputed compact keys.  NULLs sort last (any consistent placement
// groups them).
func (pt *preparedTable) comparePart(a, b int) int {
	pk := pt.pk
	for j := 0; j < pk; j++ {
		an, bn := pt.partIsNull[a*pk+j], pt.partIsNull[b*pk+j]
		if an || bn {
			if an && bn {
				continue
			}
			if an {
				return 1
			}
			return -1
		}
		if c := value.CompareKeys(pt.partSorted[a*pk+j], pt.partSorted[b*pk+j]); c != 0 {
			return c
		}
	}
	return 0
}

// samePart reports whether ordered positions a and b are in one partition.
func (pt *preparedTable) samePart(a, b int) bool {
	pk := pt.pk
	for j := 0; j < pk; j++ {
		an, bn := pt.partIsNull[a*pk+j], pt.partIsNull[b*pk+j]
		if an || bn {
			if an && bn {
				continue
			}
			return false
		}
		if value.CompareKeys(pt.partSorted[a*pk+j], pt.partSorted[b*pk+j]) != 0 {
			return false
		}
	}
	return true
}

// compareKeys compares the ORDER BY keys of ordered positions a and b.
func (pt *preparedTable) compareKeys(a, b int) int {
	k := pt.k
	for j := 0; j < k; j++ {
		an, bn := pt.keyIsNull[a*k+j], pt.keyIsNull[b*k+j]
		if an || bn {
			if an && bn {
				continue
			}
			nf := pt.keys[j].key.NullsFirst
			if an {
				if nf {
					return -1
				}
				return 1
			}
			if nf {
				return 1
			}
			return -1
		}
		c := value.CompareKeys(pt.keySorted[a*k+j], pt.keySorted[b*k+j])
		if c != 0 {
			if pt.keys[j].key.Desc {
				return -c
			}
			return c
		}
	}
	return 0
}

func comparePartition(a, b []value.Value) int {
	for i := range a {
		an, bn := a[i].IsNull(), b[i].IsNull()
		if an || bn {
			if an && bn {
				continue
			}
			// Partition grouping only needs NULLs adjacent; either consistent
			// order works, but it must put all NULLs on one side.
			if an {
				return 1 // NULLS LAST
			}
			return -1
		}
		c, err := value.Compare(a[i], b[i])
		if err != nil {
			return 0
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

// precomputePeers fills peer-group first/last positions for positions
// [base, end) and returns them partition-local.
func (pt *preparedTable) precomputePeers(base, end int, hasOrder bool) (first, last []int) {
	n := end - base
	first = make([]int, n)
	last = make([]int, n)
	if !hasOrder {
		for i := 0; i < n; i++ {
			first[i], last[i] = 0, n-1
		}
		return first, last
	}
	i := 0
	for i < n {
		j := i + 1
		for j < n && pt.compareKeys(base+j, base+i) == 0 {
			j++
		}
		for k := i; k < j; k++ {
			first[k], last[k] = i, j-1
		}
		i = j
	}
	return first, last
}

func (pt *preparedTable) String() string {
	return fmt.Sprintf("prepared(rows=%d partitions=%d keys=%d)", len(pt.order), len(pt.partStarts), len(pt.keys))
}

// sortIndexes returns the row indexes of the table in the order defined by
// keys, together with the key vectors in that order.  Ties keep input order.
func sortIndexes(t *table.Table, keys []sortKeyPlan) ([]int, error) {
	n := len(t.Rows)
	k := len(keys)
	keyVals := make([]value.Value, n*k)
	idx := make([]int, n)
	for i, row := range t.Rows {
		kv := keyVals[i*k : (i+1)*k]
		for j, key := range keys {
			v, err := key.key.Expr.Eval(row)
			if err != nil {
				return nil, err
			}
			kv[j] = v
		}
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool {
		ia, ib := idx[a], idx[b]
		if c := compareSort(keyVals[ia*k:(ia+1)*k], keyVals[ib*k:(ib+1)*k], keys); c != 0 {
			return c < 0
		}
		return ia < ib
	})
	return idx, nil
}
