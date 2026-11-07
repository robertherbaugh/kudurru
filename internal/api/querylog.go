package api

import (
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

// clusterQuerylog is the reader of questions about the whole cluster that
// are not scatter-gathered (the policy page): the central database when
// there is one (D161), else this node's log; nil when neither exists.
func (s *Server) clusterQuerylog() *sqlitelog.Reader {
	if s.opts.CentralQuerylog != nil {
		return s.opts.CentralQuerylog
	}
	return s.localQuerylog()
}

func (s *Server) localQuerylog() *sqlitelog.Reader {
	if s.opts.QuerylogSource != nil {
		return s.opts.QuerylogSource()
	}
	return s.opts.Querylog
}
