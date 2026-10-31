package sqlitelog

import "strings"

// This is an index of retained facts, not a completed historical rollup. It
// changes in the fact transaction, including rollback and privacy decisions.
// Deletes leave conservative extrema: they may require extra boundary reads,
// but the counters always describe exactly the remaining facts.
var domainHourSchema = []string{
	`CREATE TABLE query_domain_hour (
 ts INTEGER NOT NULL, node INTEGER NOT NULL REFERENCES dict_node,
 domain INTEGER NOT NULL REFERENCES dict_domain, total INTEGER NOT NULL,
 blocked INTEGER NOT NULL, first_ts INTEGER NOT NULL, last_ts INTEGER NOT NULL,
 PRIMARY KEY(ts,node,domain)) WITHOUT ROWID`,
	`INSERT INTO query_domain_hour SELECT (ts/3600000000)*3600,node,domain,count(*),sum(status BETWEEN 16 AND 23),min(ts),max(ts) FROM query GROUP BY 1,2,3`,
	`CREATE TRIGGER query_domain_insert AFTER INSERT ON query BEGIN
 INSERT INTO query_domain_hour VALUES((NEW.ts/3600000000)*3600,NEW.node,NEW.domain,1,NEW.status BETWEEN 16 AND 23,NEW.ts,NEW.ts)
 ON CONFLICT(ts,node,domain) DO UPDATE SET total=total+1,blocked=blocked+excluded.blocked,first_ts=min(first_ts,excluded.first_ts),last_ts=max(last_ts,excluded.last_ts);
 END`,
	`CREATE TRIGGER query_domain_delete AFTER DELETE ON query BEGIN
 UPDATE query_domain_hour SET total=total-1,blocked=blocked-(OLD.status BETWEEN 16 AND 23)
 WHERE ts=(OLD.ts/3600000000)*3600 AND node=OLD.node AND domain=OLD.domain;
 DELETE FROM query_domain_hour WHERE ts=(OLD.ts/3600000000)*3600 AND node=OLD.node AND domain=OLD.domain AND total=0;
 END`,
}

// domainFactParts reads complete domain/hour entries directly and visits raw
// facts only for entries cut by an edge. The boundary relation drives indexed
// domain/time lookups, so an empty boundary never scans the current hour.
func domainFactParts(tx *rtx, sg segment, step int64, args *[]any) []string {
	from, until := sg.from*1e6, sg.until*1e6
	*args = append(*args, floorTo(sg.from, 3600), ceilTo(sg.until, 3600), from, until)
	whole := `SELECT ` + strings.ReplaceAll(bucketExpr(step, false), "ts", "l.ts") + ` AS bk,l.domain AS k,l.total AS a,l.blocked AS b FROM query_domain_hour l WHERE l.ts>=? AND l.ts<? AND l.first_ts>=? AND l.last_ts<?` + tx.nodeWhere("l.", args)
	*args = append(*args, floorTo(sg.from, 3600), ceilTo(sg.until, 3600), from, until)
	edgeWhere := `l.ts>=? AND l.ts<? AND (l.first_ts<? OR l.last_ts>=?)` + tx.nodeWhere("l.", args)
	*args = append(*args, from, until)
	factsWhere := `q.domain=l.domain AND q.node=l.node AND q.ts>=l.ts*1000000 AND q.ts<(l.ts+3600)*1000000 AND q.ts>=? AND q.ts<?` + tx.nodeWhere("q.", args)
	relation := `query_domain_hour l CROSS JOIN query q INDEXED BY query_domain_ts WHERE ` + edgeWhere + ` AND ` + factsWhere
	if tx.d == dialectPostgres {
		// OFFSET 0 keeps the parameterized lookup behind the filtered edge cells.
		// The source predicate prevents same-node source files from overlapping.
		relation = `(SELECT * FROM query_domain_hour l WHERE ` + edgeWhere + `) l CROSS JOIN LATERAL (SELECT * FROM query q WHERE ` + factsWhere + ` AND q.source=l.source OFFSET 0) q`
	}
	edge := `SELECT ` + strings.ReplaceAll(bucketExpr(step, true), "ts", "q.ts") + ` AS bk,q.domain AS k,1 AS a,CASE WHEN q.status BETWEEN 16 AND 23 THEN 1 ELSE 0 END AS b FROM ` + relation
	return []string{whole, edge}
}
