/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package optimize

import (
	"context"
	"strings"
)

import (
	"github.com/pkg/errors"
)

import (
	"github.com/arana-db/arana/pkg/dataset"
	"github.com/arana-db/arana/pkg/merge/aggregator"
	"github.com/arana-db/arana/pkg/proto"
	"github.com/arana-db/arana/pkg/proto/rule"
	"github.com/arana-db/arana/pkg/runtime/ast"
	rcontext "github.com/arana-db/arana/pkg/runtime/context"
	"github.com/arana-db/arana/pkg/runtime/plan"
	"github.com/arana-db/arana/pkg/transformer"
	"github.com/arana-db/arana/pkg/util/log"
)

const (
	_bypass uint32 = 1 << iota
	_supported
)

func init() {
	registerOptimizeHandler(ast.SQLTypeSelect, optimizeSelect)
}

func optimizeSelect(ctx context.Context, o *optimizer) (proto.Plan, error) {
	stmt := o.stmt.(*ast.SelectStatement)

	// overwrite stmt limit x offset y. eg `select * from student offset 100 limit 5` will be
	// `select * from student offset 0 limit 100+5`
	originOffset, newLimit := overwriteLimit(stmt, &o.args)
	if stmt.HasJoin() {
		return optimizeJoin(o, stmt)
	}
	flag := getSelectFlag(o.rule, stmt)
	if flag&_supported == 0 {
		return nil, errors.Errorf("unsupported sql: %s", rcontext.SQL(ctx))
	}

	if flag&_bypass != 0 {
		if len(stmt.From) > 0 {
			err := rewriteSelectStatement(ctx, o, stmt, rcontext.DBGroup(ctx), stmt.From[0].TableName().Suffix())
			if err != nil {
				return nil, err
			}
		}
		ret := &plan.SimpleQueryPlan{Stmt: stmt}
		ret.BindArgs(o.args)
		return ret, nil
	}

	var (
		shards   rule.DatabaseTables
		fullScan bool
		err      error
		vt       = o.rule.MustVTable(stmt.From[0].TableName().Suffix())
	)

	if shards, fullScan, err = (*Sharder)(o.rule).Shard(stmt.From[0].TableName(), stmt.Where, o.args...); err != nil {
		return nil, errors.Wrap(err, "calculate shards failed")
	}

	log.Debugf("compute shards: result=%s, isFullScan=%v", shards, fullScan)

	// return error if full-scan is disabled
	if fullScan && !vt.AllowFullScan() {
		return nil, errors.WithStack(errDenyFullScan)
	}

	toSingle := func(db, tbl string) (proto.Plan, error) {
		if err := rewriteSelectStatement(ctx, o, stmt, db, tbl); err != nil {
			return nil, err
		}
		ret := &plan.SimpleQueryPlan{
			Stmt:     stmt,
			Database: db,
			Tables:   []string{tbl},
		}
		ret.BindArgs(o.args)

		return ret, nil
	}

	// Go through first table if no shards matched.
	// For example:
	//    SELECT ... FROM xxx WHERE a > 8 and a < 4
	if shards.IsEmpty() {
		var (
			db0, tbl0 string
			ok        bool
		)
		if db0, tbl0, ok = vt.Topology().Render(0, 0); !ok {
			return nil, errors.Errorf("cannot compute minimal topology from '%s'", stmt.From[0].TableName().Suffix())
		}

		return toSingle(db0, tbl0)
	}

	// Handle single shard
	if shards.Len() == 1 {
		var db, tbl string
		for k, v := range shards {
			db = k
			tbl = v[0]
		}
		return toSingle(db, tbl)
	}

	// Handle multiple shards

	if shards.IsFullScan() { // expand all shards if all shards matched
		// init shards
		shards = rule.DatabaseTables{}
		// compute all tables
		topology := vt.Topology()
		topology.Each(func(dbIdx, tbIdx int) bool {
			if d, t, ok := topology.Render(dbIdx, tbIdx); ok {
				shards[d] = append(shards[d], t)
			}
			return true
		})
	}

	plans := make([]proto.Plan, 0, len(shards))
	for k, v := range shards {
		next := &plan.SimpleQueryPlan{
			Database: k,
			Tables:   v,
			Stmt:     stmt,
		}
		next.BindArgs(o.args)
		plans = append(plans, next)
	}

	if len(plans) > 0 {
		tempPlan := plans[0].(*plan.SimpleQueryPlan)
		if err = rewriteSelectStatement(ctx, o, stmt, tempPlan.Database, tempPlan.Tables[0]); err != nil {
			return nil, err
		}
	}

	var tmpPlan proto.Plan
	tmpPlan = &plan.UnionPlan{
		Plans: plans,
	}

	if stmt.Limit != nil {
		tmpPlan = &plan.LimitPlan{
			ParentPlan:     tmpPlan,
			OriginOffset:   originOffset,
			OverwriteLimit: newLimit,
		}
	}

	orderByItems := optimizeOrderBy(stmt)

	if stmt.OrderBy != nil {
		tmpPlan = &plan.OrderPlan{
			ParentPlan:   tmpPlan,
			OrderByItems: orderByItems,
		}
	}

	convertOrderByItems := func(origins []*ast.OrderByItem) []dataset.OrderByItem {
		var result = make([]dataset.OrderByItem, 0, len(origins))
		for _, origin := range origins {
			var columnName string
			if cn, ok := origin.Expr.(ast.ColumnNameExpressionAtom); ok {
				columnName = cn.Suffix()
			}
			result = append(result, dataset.OrderByItem{
				Column: columnName,
				Desc:   origin.Desc,
			})
		}
		return result
	}
	if stmt.GroupBy != nil {
		return &plan.GroupPlan{
			Plan:         tmpPlan,
			AggItems:     aggregator.LoadAggs(stmt.Select),
			OrderByItems: convertOrderByItems(stmt.OrderBy),
		}, nil
	} else {
		// TODO: refactor groupby/orderby/aggregate plan to a unified plan
		return &plan.AggregatePlan{
			Plan:       tmpPlan,
			Combiner:   transformer.NewCombinerManager(),
			AggrLoader: transformer.LoadAggrs(stmt.Select),
		}, nil
	}
}

//optimizeJoin ony support  a join b in one db
func optimizeJoin(o *optimizer, stmt *ast.SelectStatement) (proto.Plan, error) {
	join := stmt.From[0].Source().(*ast.JoinNode)

	compute := func(tableSource *ast.TableSourceNode) (database, alias string, shardList []string, err error) {
		table := tableSource.TableName()
		if table == nil {
			err = errors.New("must table, not statement or join node")
			return
		}
		alias = tableSource.Alias()
		database = table.Prefix()

		shards, err := o.computeShards(table, nil, o.args)
		if err != nil {
			return
		}
		//table no shard
		if shards == nil {
			shardList = append(shardList, table.Suffix())
			return
		}
		//table  shard more than one db
		if len(shards) > 1 {
			err = errors.New("not support more than one db")
			return
		}

		for k, v := range shards {
			database = k
			shardList = v
		}

		if alias == "" {
			alias = table.Suffix()
		}

		return
	}

	dbLeft, aliasLeft, shardLeft, err := compute(join.Left)
	if err != nil {
		return nil, err
	}
	dbRight, aliasRight, shardRight, err := compute(join.Right)

	if err != nil {
		return nil, err
	}

	if dbLeft != "" && dbRight != "" && dbLeft != dbRight {
		return nil, errors.New("not support more than one db")
	}

	joinPan := &plan.SimpleJoinPlan{
		Left: &plan.JoinTable{
			Tables: shardLeft,
			Alias:  aliasLeft,
		},
		Join: join,
		Right: &plan.JoinTable{
			Tables: shardRight,
			Alias:  aliasRight,
		},
		Stmt: o.stmt.(*ast.SelectStatement),
	}
	joinPan.BindArgs(o.args)

	return joinPan, nil
}

func getSelectFlag(ru *rule.Rule, stmt *ast.SelectStatement) (flag uint32) {
	switch len(stmt.From) {
	case 1:
		from := stmt.From[0]
		tn := from.TableName()

		if tn == nil { // only FROM table supported now
			return
		}

		flag |= _supported

		if len(tn) > 1 {
			switch strings.ToLower(tn.Prefix()) {
			case "mysql", "information_schema":
				flag |= _bypass
				return
			}
		}
		if !ru.Has(tn.Suffix()) {
			flag |= _bypass
		}
	case 0:
		flag |= _bypass
		flag |= _supported
	}
	return
}

func optimizeOrderBy(stmt *ast.SelectStatement) []dataset.OrderByItem {
	if stmt == nil || stmt.OrderBy == nil {
		return nil
	}
	result := make([]dataset.OrderByItem, 0, len(stmt.OrderBy))
	for _, node := range stmt.OrderBy {
		column, _ := node.Expr.(ast.ColumnNameExpressionAtom)
		item := dataset.OrderByItem{
			Column: column[0],
			Desc:   node.Desc,
		}
		result = append(result, item)
	}
	return result
}

func overwriteLimit(stmt *ast.SelectStatement, args *[]interface{}) (originOffset, overwriteLimit int64) {
	if stmt == nil || stmt.Limit == nil {
		return 0, 0
	}

	offset := stmt.Limit.Offset()
	limit := stmt.Limit.Limit()

	// SELECT * FROM student where uid = ? limit ? offset ?
	var offsetIndex int64
	var limitIndex int64

	if stmt.Limit.IsOffsetVar() {
		offsetIndex = offset
		offset = (*args)[offsetIndex].(int64)

		if !stmt.Limit.IsLimitVar() {
			limit = stmt.Limit.Limit()
			*args = append(*args, limit)
			limitIndex = int64(len(*args) - 1)
		}
	}
	originOffset = offset

	if stmt.Limit.IsLimitVar() {
		limitIndex = limit
		limit = (*args)[limitIndex].(int64)

		if !stmt.Limit.IsOffsetVar() {
			*args = append(*args, int64(0))
			offsetIndex = int64(len(*args) - 1)
		}
	}

	if stmt.Limit.IsLimitVar() || stmt.Limit.IsOffsetVar() {
		if !stmt.Limit.IsLimitVar() {
			stmt.Limit.SetLimitVar()
			stmt.Limit.SetLimit(limitIndex)
		}
		if !stmt.Limit.IsOffsetVar() {
			stmt.Limit.SetOffsetVar()
			stmt.Limit.SetOffset(offsetIndex)
		}

		newLimitVar := limit + offset
		overwriteLimit = newLimitVar
		(*args)[limitIndex] = newLimitVar
		(*args)[offsetIndex] = int64(0)
		return
	}

	stmt.Limit.SetOffset(0)
	stmt.Limit.SetLimit(offset + limit)
	overwriteLimit = offset + limit
	return
}

func rewriteSelectStatement(ctx context.Context, o *optimizer, stmt *ast.SelectStatement, db, tb string) error {
	// todo db 计算逻辑&tb shard 的计算逻辑
	var starExpand = false
	if len(stmt.Select) == 1 {
		if _, ok := stmt.Select[0].(*ast.SelectElementAll); ok {
			starExpand = true
		}
	}

	if starExpand {
		if len(tb) < 1 {
			tb = stmt.From[0].TableName().Suffix()
		}
		metaData := o.schemaLoader.Load(ctx, o.vconn, db, []string{tb})[tb]
		if metaData == nil || len(metaData.ColumnNames) == 0 {
			return errors.Errorf("can not get metadata for db:%s and table:%s", db, tb)
		}
		selectElements := make([]ast.SelectElement, len(metaData.Columns))
		for i, column := range metaData.ColumnNames {
			selectElements[i] = ast.NewSelectElementColumn([]string{column}, "")
		}
		stmt.Select = selectElements
	}

	return nil
}