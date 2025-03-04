// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package mutations

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"regexp"
	"sort"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/geo/geoindex"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/colinfo"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/stats"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
)

var (
	// StatisticsMutator adds ALTER TABLE INJECT STATISTICS statements.
	StatisticsMutator MultiStatementMutation = statisticsMutator

	// ForeignKeyMutator adds ALTER TABLE ADD FOREIGN KEY statements.
	ForeignKeyMutator MultiStatementMutation = foreignKeyMutator

	// ColumnFamilyMutator modifies a CREATE TABLE statement without any FAMILY
	// definitions to have random FAMILY definitions.
	ColumnFamilyMutator StatementMutator = rowenc.ColumnFamilyMutator

	// IndexStoringMutator modifies the STORING clause of CREATE INDEX and
	// indexes in CREATE TABLE.
	IndexStoringMutator MultiStatementMutation = rowenc.IndexStoringMutator

	// PartialIndexMutator adds random partial index predicate expressions to
	// indexes.
	PartialIndexMutator MultiStatementMutation = rowenc.PartialIndexMutator

	// PostgresMutator modifies strings such that they execute identically
	// in both Postgres and Cockroach (however this mutator does not remove
	// features not supported by Postgres; use PostgresCreateTableMutator
	// for those).
	PostgresMutator StatementStringMutator = postgresMutator

	// PostgresCreateTableMutator modifies CREATE TABLE statements to
	// remove any features not supported by Postgres that would change
	// results (like descending primary keys). This should be used on the
	// output of sqlbase.RandCreateTable.
	PostgresCreateTableMutator MultiStatementMutation = postgresCreateTableMutator
)

var (
	// These are used in pkg/compose/compare/compare/compare_test.go, but
	// it has a build tag so it's not detected by the linter.
	_ = IndexStoringMutator
	_ = PostgresCreateTableMutator
)

// StatementMutator defines a func that can change a statement.
type StatementMutator func(rng *rand.Rand, stmt tree.Statement) (changed bool)

// MultiStatementMutation defines a func that can return a list of new and/or mutated statements.
type MultiStatementMutation func(rng *rand.Rand, stmts []tree.Statement) (mutated []tree.Statement, changed bool)

// Mutate implements the Mutator interface.
func (sm StatementMutator) Mutate(
	rng *rand.Rand, stmts []tree.Statement,
) (mutated []tree.Statement, changed bool) {
	for _, stmt := range stmts {
		sc := sm(rng, stmt)
		changed = changed || sc
	}
	return stmts, changed
}

// Mutate implements the Mutator interface.
func (msm MultiStatementMutation) Mutate(
	rng *rand.Rand, stmts []tree.Statement,
) (mutated []tree.Statement, changed bool) {
	return msm(rng, stmts)
}

// Apply executes all mutators on stmts. It returns the (possibly mutated and
// changed in place) statements and a boolean indicating whether any changes
// were made.
func Apply(
	rng *rand.Rand, stmts []tree.Statement, mutators ...rowenc.Mutator,
) (mutated []tree.Statement, changed bool) {
	var mc bool
	for _, m := range mutators {
		stmts, mc = m.Mutate(rng, stmts)
		changed = changed || mc
	}
	return stmts, changed
}

// StringMutator defines a mutator that works on strings.
type StringMutator interface {
	MutateString(*rand.Rand, string) (mutated string, changed bool)
}

// StatementStringMutator defines a func that mutates a string.
type StatementStringMutator func(*rand.Rand, string) string

// Mutate implements the Mutator interface.
func (sm StatementStringMutator) Mutate(
	rng *rand.Rand, stmts []tree.Statement,
) (mutated []tree.Statement, changed bool) {
	panic("can only be used with MutateString")
}

// MutateString implements the StringMutator interface.
func (sm StatementStringMutator) MutateString(
	rng *rand.Rand, q string,
) (mutated string, changed bool) {
	newq := sm(rng, q)
	return newq, newq != q
}

// ApplyString executes all mutators on input. A mutator can also be a
// StringMutator which will operate after all other mutators.
func ApplyString(
	rng *rand.Rand, input string, mutators ...rowenc.Mutator,
) (output string, changed bool) {
	parsed, err := parser.Parse(input)
	if err != nil {
		return input, false
	}

	stmts := make([]tree.Statement, len(parsed))
	for i, p := range parsed {
		stmts[i] = p.AST
	}

	var normalMutators []rowenc.Mutator
	var stringMutators []StringMutator
	for _, m := range mutators {
		if sm, ok := m.(StringMutator); ok {
			stringMutators = append(stringMutators, sm)
		} else {
			normalMutators = append(normalMutators, m)
		}
	}
	stmts, changed = Apply(rng, stmts, normalMutators...)
	if changed {
		var sb strings.Builder
		for _, s := range stmts {
			sb.WriteString(tree.Serialize(s))
			sb.WriteString(";\n")
		}
		input = sb.String()
	}
	for _, m := range stringMutators {
		s, ch := m.MutateString(rng, input)
		if ch {
			input = s
			changed = true
		}
	}
	return input, changed
}

// randNonNegInt returns a random non-negative integer. It attempts to
// distribute it over powers of 10.
func randNonNegInt(rng *rand.Rand) int64 {
	var v int64
	if n := rng.Intn(20); n == 0 {
		// v == 0
	} else if n <= 10 {
		v = rng.Int63n(10) + 1
		for i := 0; i < n; i++ {
			v *= 10
		}
	} else {
		v = rng.Int63()
	}
	return v
}

func statisticsMutator(
	rng *rand.Rand, stmts []tree.Statement,
) (mutated []tree.Statement, changed bool) {
	for _, stmt := range stmts {
		create, ok := stmt.(*tree.CreateTable)
		if !ok {
			continue
		}
		alter := &tree.AlterTable{
			Table: create.Table.ToUnresolvedObjectName(),
		}
		rowCount := randNonNegInt(rng)
		cols := map[tree.Name]*tree.ColumnTableDef{}
		colStats := map[tree.Name]*stats.JSONStatistic{}
		makeHistogram := func(col *tree.ColumnTableDef) {
			// If an index appeared before a column definition, col
			// can be nil.
			if col == nil {
				return
			}
			// Do not create a histogram 20% of the time.
			if rng.Intn(5) == 0 {
				return
			}
			colType := tree.MustBeStaticallyKnownType(col.Type)
			h := randHistogram(rng, colType)
			stat := colStats[col.Name]
			if err := stat.SetHistogram(&h); err != nil {
				panic(err)
			}
		}
		for _, def := range create.Defs {
			switch def := def.(type) {
			case *tree.ColumnTableDef:
				var nullCount, distinctCount uint64
				if rowCount > 0 {
					if def.Nullable.Nullability != tree.NotNull {
						nullCount = uint64(rng.Int63n(rowCount))
					}
					distinctCount = uint64(rng.Int63n(rowCount))
				}
				cols[def.Name] = def
				colStats[def.Name] = &stats.JSONStatistic{
					Name:          "__auto__",
					CreatedAt:     "2000-01-01 00:00:00+00:00",
					RowCount:      uint64(rowCount),
					Columns:       []string{def.Name.String()},
					DistinctCount: distinctCount,
					NullCount:     nullCount,
				}
				if (def.Unique.IsUnique && !def.Unique.WithoutIndex) || def.PrimaryKey.IsPrimaryKey {
					makeHistogram(def)
				}
			case *tree.IndexTableDef:
				// TODO(mgartner): We should make a histogram for each indexed
				// column.
				makeHistogram(cols[def.Columns[0].Column])
			case *tree.UniqueConstraintTableDef:
				if !def.WithoutIndex {
					// TODO(mgartner): We should make a histogram for each
					// column in the unique constraint.
					makeHistogram(cols[def.Columns[0].Column])
				}
			}
		}
		if len(colStats) > 0 {
			var allStats []*stats.JSONStatistic
			for _, cs := range colStats {
				allStats = append(allStats, cs)
			}
			b, err := json.Marshal(allStats)
			if err != nil {
				// Should not happen.
				panic(err)
			}
			j, err := tree.ParseDJSON(string(b))
			if err != nil {
				panic(err)
			}
			alter.Cmds = append(alter.Cmds, &tree.AlterTableInjectStats{
				Stats: j,
			})
			stmts = append(stmts, alter)
			changed = true
		}
	}
	return stmts, changed
}

// randHistogram generates a histogram for the given type with random histogram
// buckets. If colType is inverted indexable then the histogram bucket upper
// bounds are byte-encoded inverted index keys.
func randHistogram(rng *rand.Rand, colType *types.T) stats.HistogramData {
	histogramColType := colType
	if colinfo.ColumnTypeIsInvertedIndexable(colType) {
		histogramColType = types.Bytes
	}
	h := stats.HistogramData{
		ColumnType: histogramColType,
	}

	// Generate random values for histogram bucket upper bounds.
	var encodedUpperBounds [][]byte
	for i, numDatums := 0, rng.Intn(10); i < numDatums; i++ {
		upper := rowenc.RandDatum(rng, colType, false /* nullOk */)
		if colinfo.ColumnTypeIsInvertedIndexable(colType) {
			encs := encodeInvertedIndexHistogramUpperBounds(colType, upper)
			encodedUpperBounds = append(encodedUpperBounds, encs...)
		} else {
			enc, err := rowenc.EncodeTableKey(nil, upper, encoding.Ascending)
			if err != nil {
				panic(err)
			}
			encodedUpperBounds = append(encodedUpperBounds, enc)
		}
	}

	// Return early if there are no upper-bounds.
	if len(encodedUpperBounds) == 0 {
		return h
	}

	// Sort the encoded upper-bounds.
	sort.Slice(encodedUpperBounds, func(i, j int) bool {
		return bytes.Compare(encodedUpperBounds[i], encodedUpperBounds[j]) < 0
	})

	// Remove duplicates.
	dedupIdx := 1
	for i := 1; i < len(encodedUpperBounds); i++ {
		if !bytes.Equal(encodedUpperBounds[i], encodedUpperBounds[i-1]) {
			encodedUpperBounds[dedupIdx] = encodedUpperBounds[i]
			dedupIdx++
		}
	}
	encodedUpperBounds = encodedUpperBounds[:dedupIdx]

	// Create a histogram bucket for each encoded upper-bound.
	for i := range encodedUpperBounds {
		// The first bucket must have NumRange = 0, and thus DistinctRange = 0
		// as well.
		var numRange int64
		var distinctRange float64
		if i > 0 {
			numRange, distinctRange = randNumRangeAndDistinctRange(rng)
		}

		h.Buckets = append(h.Buckets, stats.HistogramData_Bucket{
			NumEq:         randNonNegInt(rng),
			NumRange:      numRange,
			DistinctRange: distinctRange,
			UpperBound:    encodedUpperBounds[i],
		})
	}

	return h
}

// encodeInvertedIndexHistogramUpperBounds returns a slice of byte-encoded
// inverted index keys that are created from val.
func encodeInvertedIndexHistogramUpperBounds(colType *types.T, val tree.Datum) (encs [][]byte) {
	var keys [][]byte
	var err error
	switch colType.Family() {
	case types.GeometryFamily:
		tempIdx := descpb.IndexDescriptor{
			GeoConfig: *geoindex.DefaultGeometryIndexConfig(),
		}
		keys, err = rowenc.EncodeGeoInvertedIndexTableKeys(val, nil, &tempIdx)
	case types.GeographyFamily:
		tempIdx := descpb.IndexDescriptor{
			GeoConfig: *geoindex.DefaultGeographyIndexConfig(),
		}
		keys, err = rowenc.EncodeGeoInvertedIndexTableKeys(val, nil, &tempIdx)
	default:
		keys, err = rowenc.EncodeInvertedIndexTableKeys(val, nil, descpb.EmptyArraysInInvertedIndexesVersion)
	}

	if err != nil {
		panic(err)
	}

	var da rowenc.DatumAlloc
	for i := range keys {
		// Each key much be a byte-encoded datum so that it can be
		// decoded in JSONStatistic.SetHistogram.
		enc, err := rowenc.EncodeTableKey(nil, da.NewDBytes(tree.DBytes(keys[i])), encoding.Ascending)
		if err != nil {
			panic(err)
		}
		encs = append(encs, enc)
	}
	return encs
}

// randNumRangeAndDistinctRange returns two random numbers to be used for
// NumRange and DistinctRange fields of a histogram bucket.
func randNumRangeAndDistinctRange(rng *rand.Rand) (numRange int64, distinctRange float64) {
	numRange = randNonNegInt(rng)
	// distinctRange should be <= numRange.
	switch rng.Intn(3) {
	case 0:
		distinctRange = 0
	case 1:
		distinctRange = float64(numRange)
	default:
		distinctRange = rng.Float64() * float64(numRange)
	}
	return numRange, distinctRange
}

// foreignKeyMutator is a MultiStatementMutation implementation which adds
// foreign key references between existing columns.
func foreignKeyMutator(
	rng *rand.Rand, stmts []tree.Statement,
) (mutated []tree.Statement, changed bool) {
	// Find columns in the tables.
	cols := map[tree.TableName][]*tree.ColumnTableDef{}
	byName := map[tree.TableName]*tree.CreateTable{}

	// Keep track of referencing columns since we have a limitation that a
	// column can only be used by one FK.
	usedCols := map[tree.TableName]map[tree.Name]bool{}

	// Keep track of table dependencies to prevent circular dependencies.
	dependsOn := map[tree.TableName]map[tree.TableName]bool{}

	var tables []*tree.CreateTable
	for _, stmt := range stmts {
		table, ok := stmt.(*tree.CreateTable)
		if !ok {
			continue
		}
		tables = append(tables, table)
		byName[table.Table] = table
		usedCols[table.Table] = map[tree.Name]bool{}
		dependsOn[table.Table] = map[tree.TableName]bool{}
		for _, def := range table.Defs {
			switch def := def.(type) {
			case *tree.ColumnTableDef:
				cols[table.Table] = append(cols[table.Table], def)
			}
		}
	}
	if len(tables) == 0 {
		return stmts, false
	}

	toNames := func(cols []*tree.ColumnTableDef) tree.NameList {
		names := make(tree.NameList, len(cols))
		for i, c := range cols {
			names[i] = c.Name
		}
		return names
	}

	// We cannot mutate the table definitions themselves because 1) we
	// don't know the order of dependencies (i.e., table 1 could reference
	// table 4 which doesn't exist yet) and relatedly 2) we don't prevent
	// circular dependencies. Instead, add new ALTER TABLE commands to the
	// end of a list of statements.

	// Create some FKs.
	for rng.Intn(2) == 0 {
		// Choose a random table.
		table := tables[rng.Intn(len(tables))]
		// Choose a random column subset.
		var fkCols []*tree.ColumnTableDef
		for _, c := range cols[table.Table] {
			if c.Computed.Computed {
				// We don't support FK references from computed columns (#46672).
				continue
			}
			if usedCols[table.Table][c.Name] {
				continue
			}
			fkCols = append(fkCols, c)
		}
		if len(fkCols) == 0 {
			continue
		}
		rng.Shuffle(len(fkCols), func(i, j int) {
			fkCols[i], fkCols[j] = fkCols[j], fkCols[i]
		})
		// Pick some randomly short prefix. I'm sure there's a closed
		// form solution to this with a single call to rng.Intn but I'm
		// not sure what to search for.
		i := 1
		for len(fkCols) > i && rng.Intn(2) == 0 {
			i++
		}
		fkCols = fkCols[:i]

		// Check if a table has the needed column types.
	LoopTable:
		for refTable, refCols := range cols {
			// Prevent circular and self references because
			// generating valid INSERTs could become impossible or
			// difficult algorithmically.
			if refTable == table.Table || len(refCols) < len(fkCols) {
				continue
			}

			{
				// To prevent circular references, find all transitive
				// dependencies of refTable and make sure none of them
				// are table.
				stack := []tree.TableName{refTable}
				for i := 0; i < len(stack); i++ {
					curTable := stack[i]
					if curTable == table.Table {
						// table was trying to add a dependency
						// to refTable, but refTable already
						// depends on table (directly or
						// indirectly).
						continue LoopTable
					}
					for t := range dependsOn[curTable] {
						stack = append(stack, t)
					}
				}
			}

			// We found a table with enough columns. Check if it
			// has some columns that are needed types. In order
			// to not use columns multiple times, keep track of
			// available columns.
			availCols := append([]*tree.ColumnTableDef(nil), refCols...)
			var usingCols []*tree.ColumnTableDef
			for len(availCols) > 0 && len(usingCols) < len(fkCols) {
				fkCol := fkCols[len(usingCols)]
				found := false
				for refI, refCol := range availCols {
					if refCol.Computed.Virtual {
						// We don't support FK references to virtual columns (#51296).
						continue
					}
					fkColType := tree.MustBeStaticallyKnownType(fkCol.Type)
					refColType := tree.MustBeStaticallyKnownType(refCol.Type)
					if fkColType.Equivalent(refColType) && colinfo.ColumnTypeIsIndexable(refColType) {
						usingCols = append(usingCols, refCol)
						availCols = append(availCols[:refI], availCols[refI+1:]...)
						found = true
						break
					}
				}
				if !found {
					continue LoopTable
				}
			}
			// If we didn't find enough columns, try another table.
			if len(usingCols) != len(fkCols) {
				continue
			}

			// Found a suitable table.
			// TODO(mjibson): prevent the creation of unneeded
			// unique indexes. One may already exist with the
			// correct prefix.
			ref := byName[refTable]
			refColumns := make(tree.IndexElemList, len(usingCols))
			for i, c := range usingCols {
				refColumns[i].Column = c.Name
			}
			for _, c := range fkCols {
				usedCols[table.Table][c.Name] = true
			}
			dependsOn[table.Table][ref.Table] = true
			ref.Defs = append(ref.Defs, &tree.UniqueConstraintTableDef{
				IndexTableDef: tree.IndexTableDef{
					Columns: refColumns,
				},
			})

			match := tree.MatchSimple
			// TODO(mjibson): Set match once #42498 is fixed.
			var actions tree.ReferenceActions
			if rng.Intn(2) == 0 {
				actions.Delete = randAction(rng, table)
			}
			if rng.Intn(2) == 0 {
				actions.Update = randAction(rng, table)
			}
			stmts = append(stmts, &tree.AlterTable{
				Table: table.Table.ToUnresolvedObjectName(),
				Cmds: tree.AlterTableCmds{&tree.AlterTableAddConstraint{
					ConstraintDef: &tree.ForeignKeyConstraintTableDef{
						Table:    ref.Table,
						FromCols: toNames(fkCols),
						ToCols:   toNames(usingCols),
						Actions:  actions,
						Match:    match,
					},
				}},
			})
			changed = true
			break
		}
	}

	return stmts, changed
}

func randAction(rng *rand.Rand, table *tree.CreateTable) tree.ReferenceAction {
	const highestAction = tree.Cascade
	// Find a valid action. Depending on the random action chosen, we have
	// to verify some validity conditions.
Loop:
	for {
		action := tree.ReferenceAction(rng.Intn(int(highestAction + 1)))
		for _, def := range table.Defs {
			col, ok := def.(*tree.ColumnTableDef)
			if !ok {
				continue
			}
			switch action {
			case tree.SetNull:
				if col.Nullable.Nullability == tree.NotNull {
					continue Loop
				}
			case tree.SetDefault:
				if col.DefaultExpr.Expr == nil && col.Nullable.Nullability == tree.NotNull {
					continue Loop
				}
			}
		}
		return action
	}
}

var postgresMutatorAtIndex = regexp.MustCompile(`@[\[\]\w]+`)

func postgresMutator(rng *rand.Rand, q string) string {
	q, _ = ApplyString(rng, q, postgresStatementMutator)

	for from, to := range map[string]string{
		":::":     "::",
		"STRING":  "TEXT",
		"BYTES":   "BYTEA",
		"FLOAT4":  "FLOAT8",
		"INT2":    "INT8",
		"INT4":    "INT8",
		"STORING": "INCLUDE",
		" AS (":   " GENERATED ALWAYS AS (",
		",)":      ")",
	} {
		q = strings.Replace(q, from, to, -1)
	}
	q = postgresMutatorAtIndex.ReplaceAllString(q, "")
	return q
}

// postgresStatementMutator removes cockroach-only things from CREATE TABLE and
// ALTER TABLE.
var postgresStatementMutator MultiStatementMutation = func(rng *rand.Rand, stmts []tree.Statement) (mutated []tree.Statement, changed bool) {
	for _, stmt := range stmts {
		switch stmt := stmt.(type) {
		case *tree.SetClusterSetting, *tree.SetVar:
			continue
		case *tree.CreateTable:
			if stmt.Interleave != nil {
				stmt.Interleave = nil
				changed = true
			}
			if stmt.PartitionByTable != nil {
				stmt.PartitionByTable = nil
				changed = true
			}
			for i := 0; i < len(stmt.Defs); i++ {
				switch def := stmt.Defs[i].(type) {
				case *tree.FamilyTableDef:
					// Remove.
					stmt.Defs = append(stmt.Defs[:i], stmt.Defs[i+1:]...)
					i--
					changed = true
				case *tree.ColumnTableDef:
					if def.HasColumnFamily() {
						def.Family.Name = ""
						def.Family.Create = false
						changed = true
					}
					if def.Unique.WithoutIndex {
						def.Unique.WithoutIndex = false
						changed = true
					}
					if def.IsVirtual() {
						def.Computed.Virtual = false
						def.Computed.Computed = true
						changed = true
					}
				case *tree.UniqueConstraintTableDef:
					if def.Interleave != nil {
						def.Interleave = nil
						changed = true
					}
					if def.PartitionByIndex != nil {
						def.PartitionByIndex = nil
						changed = true
					}
					if def.WithoutIndex {
						def.WithoutIndex = false
						changed = true
					}
				}
			}
		case *tree.AlterTable:
			for i := 0; i < len(stmt.Cmds); i++ {
				// Postgres doesn't have alter stats.
				if _, ok := stmt.Cmds[i].(*tree.AlterTableInjectStats); ok {
					stmt.Cmds = append(stmt.Cmds[:i], stmt.Cmds[i+1:]...)
					i--
					changed = true
				}
			}
			// If there are no commands, don't add this statement.
			if len(stmt.Cmds) == 0 {
				continue
			}
		}
		mutated = append(mutated, stmt)
	}
	return mutated, changed
}

func postgresCreateTableMutator(
	rng *rand.Rand, stmts []tree.Statement,
) (mutated []tree.Statement, changed bool) {
	for _, stmt := range stmts {
		mutated = append(mutated, stmt)
		switch stmt := stmt.(type) {
		case *tree.CreateTable:
			if stmt.Interleave != nil {
				stmt.Interleave = nil
				changed = true
			}
			// Get all the column types first.
			colTypes := make(map[string]*types.T)
			for _, def := range stmt.Defs {
				switch def := def.(type) {
				case *tree.ColumnTableDef:
					colTypes[string(def.Name)] = tree.MustBeStaticallyKnownType(def.Type)
				}
			}

			var newdefs tree.TableDefs
			for _, def := range stmt.Defs {
				switch def := def.(type) {
				case *tree.IndexTableDef:
					// Postgres doesn't support indexes in CREATE TABLE, so split them out
					// to their own statement.
					var newCols tree.IndexElemList
					for _, col := range def.Columns {
						// Postgres doesn't support box2d as a btree index key.
						colTypeFamily := colTypes[string(col.Column)].Family()
						if colTypeFamily == types.Box2DFamily {
							changed = true
						} else {
							newCols = append(newCols, col)
						}
					}
					if len(newCols) == 0 {
						// Break without adding this index at all.
						break
					}
					def.Columns = newCols
					// TODO(rafi): Postgres supports inverted indexes with a different
					// syntax than Cockroach. Maybe we could add it later.
					// The syntax is `CREATE INDEX name ON table USING gin(column)`.
					if !def.Inverted {
						mutated = append(mutated, &tree.CreateIndex{
							Name:     def.Name,
							Table:    stmt.Table,
							Inverted: def.Inverted,
							Columns:  newCols,
							Storing:  def.Storing,
						})
						changed = true
					}
				case *tree.UniqueConstraintTableDef:
					var newCols tree.IndexElemList
					for _, col := range def.Columns {
						// Postgres doesn't support box2d as a btree index key.
						colTypeFamily := colTypes[string(col.Column)].Family()
						if colTypeFamily == types.Box2DFamily {
							changed = true
						} else {
							newCols = append(newCols, col)
						}
					}
					if len(newCols) == 0 {
						// Break without adding this index at all.
						break
					}
					def.Columns = newCols
					if def.PrimaryKey {
						for i, col := range def.Columns {
							// Postgres doesn't support descending PKs.
							if col.Direction != tree.DefaultDirection {
								def.Columns[i].Direction = tree.DefaultDirection
								changed = true
							}
						}
						if def.Name != "" {
							// Unset Name here because constraint names cannot be shared among
							// tables, so multiple PK constraints named "primary" is an error.
							def.Name = ""
							changed = true
						}
						newdefs = append(newdefs, def)
						break
					}
					mutated = append(mutated, &tree.CreateIndex{
						Name:     def.Name,
						Table:    stmt.Table,
						Unique:   true,
						Inverted: def.Inverted,
						Columns:  newCols,
						Storing:  def.Storing,
					})
					changed = true
				case *tree.ColumnTableDef:
					if def.IsComputed() {
						// Postgres has different cast volatility for timestamps and OID
						// types. The substitution here is specific to the output of
						// testutils.randComputedColumnTableDef.
						if funcExpr, ok := def.Computed.Expr.(*tree.FuncExpr); ok {
							if len(funcExpr.Exprs) == 1 {
								if castExpr, ok := funcExpr.Exprs[0].(*tree.CastExpr); ok {
									referencedType := colTypes[castExpr.Expr.(*tree.UnresolvedName).String()]
									isContextDependentType := referencedType.Family() == types.TimestampFamily ||
										referencedType.Family() == types.OidFamily
									if isContextDependentType &&
										tree.MustBeStaticallyKnownType(castExpr.Type) == types.String {
										def.Computed.Expr = &tree.CaseExpr{
											Whens: []*tree.When{
												{
													Cond: &tree.IsNullExpr{
														Expr: castExpr.Expr,
													},
													Val: rowenc.RandDatum(rng, types.String, true /* nullOK */),
												},
											},
											Else: rowenc.RandDatum(rng, types.String, true /* nullOK */),
										}
										changed = true
									}
								}
							}
						}
					}
					newdefs = append(newdefs, def)
				default:
					newdefs = append(newdefs, def)
				}
			}
			stmt.Defs = newdefs
		}
	}
	return mutated, changed
}
