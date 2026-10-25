package proxy

import (
	"errors"
	"testing"
)

// Statements the platform legitimately sends, or that look dangerous only
// inside a literal, a quoted identifier or a comment.
func TestValidateReadOnlyAccepts(t *testing.T) {
	for _, sql := range []string{
		"SELECT 1 FROM dual",
		"  select owner, object_name from all_objects where owner = :owner  ",
		"WITH t AS (SELECT 1 x FROM dual) SELECT x FROM t",
		"SELECT /*+ FIRST_ROWS */ 1 FROM dual",
		"-- a leading comment\nSELECT 1 FROM dual",
		"SELECT DBMS_METADATA.GET_DDL(:t, :n, :o) FROM dual",
		"SELECT sys_context('USERENV', 'CURRENT_SCHEMA') FROM dual",
		"SELECT * FROM all_objects CONNECT BY PRIOR object_id = object_id START WITH object_id = 1",
		"SELECT 'FOR UPDATE; WITH FUNCTION NEXTVAL' FROM dual",
		"SELECT 'it''s; FOR UPDATE' FROM dual",
		"SELECT q'[it's; FOR UPDATE]' FROM dual",
		"SELECT Q'{a}', nq'<b>', NQ'!c!' FROM dual",
		"SELECT n'x', DATE '2026-01-01' FROM dual",
		`SELECT "FUNCTION", "UPDATE" FROM t`,
		`WITH "FUNCTION" AS (SELECT 1 FROM dual) SELECT * FROM "FUNCTION"`,
		"SELECT text FROM all_source WHERE text LIKE '%FOR UPDATE%'",
		"SELECT 1 FROM dual -- FOR UPDATE",
		"SELECT 1 FROM dual /* ; NEXTVAL */",
		"SELECT s.CURRVAL, 'è' FROM dual",
		"SELECT data_default FROM all_tab_columns WHERE data_default LIKE '%\"NEXTVAL\"%'",
	} {
		if err := validateReadOnly(sql); err != nil {
			t.Errorf("validateReadOnly(%q) = %v, want accepted", sql, err)
		}
	}
}

// Each refusal is pinned to its reason, so a statement refused for an
// unrelated cause cannot make a case pass.
func TestValidateReadOnlyRefuses(t *testing.T) {
	cases := []struct {
		sql  string
		want error
	}{
		{"INSERT INTO t VALUES (1)", errNotAQuery},
		{"UPDATE t SET x = 1", errNotAQuery},
		{"DELETE FROM t", errNotAQuery},
		{"MERGE INTO t USING s ON (1 = 1) WHEN MATCHED THEN UPDATE SET x = 1", errNotAQuery},
		{"DROP TABLE t", errNotAQuery},
		{"BEGIN NULL; END;", errNotAQuery},
		{"DECLARE x NUMBER; BEGIN NULL; END;", errNotAQuery},
		{"CALL p()", errNotAQuery},
		{"LOCK TABLE t IN EXCLUSIVE MODE", errNotAQuery},
		{"EXPLAIN PLAN FOR SELECT 1 FROM dual", errNotAQuery},
		{"(SELECT 1 FROM dual)", errNotAQuery},
		{"SELECTX 1 FROM dual", errNotAQuery},
		{"", errNotAQuery},
		{"  -- only a comment", errNotAQuery},

		// Inline PL/SQL: the form that writes, since an autonomous transaction
		// commits on its own.
		{"WITH FUNCTION f RETURN NUMBER IS PRAGMA AUTONOMOUS_TRANSACTION; BEGIN " +
			"EXECUTE IMMEDIATE 'DROP TABLE t'; RETURN 1; END; SELECT f FROM dual", errInlinePLSQL},
		// Without the keyword pair, the semicolons PL/SQL cannot do without
		// still refuse it.
		{"WITH x AS (SELECT 1 FROM dual) SELECT 1 FROM x; BEGIN NULL; END", errSemicolon},
		{"with /* hidden */ procedure p is begin null end select 1 from dual", errInlinePLSQL},
		{"SELECT /*+ WITH_PLSQL */ * FROM (WITH FUNCTION f RETURN NUMBER IS BEGIN RETURN 1 END " +
			"SELECT f() FROM dual)", errInlinePLSQL},

		// Row locks.
		{"SELECT * FROM t FOR UPDATE", errForUpdate},
		{"select * from t for update nowait", errForUpdate},
		{"SELECT * FROM t FOR--split\nUPDATE", errForUpdate},
		{"SELECT * FROM t FOR /* split */ UPDATE OF c SKIP LOCKED", errForUpdate},
		{"SELECT * FROM t -- comment\rFOR UPDATE", errForUpdate},
		// A q-quoted literal read as two plain ones would put FOR UPDATE inside
		// a literal: the lexer must see it as code, as the database does.
		{"SELECT q'[x']' FROM t FOR UPDATE --'", errForUpdate},

		// Sequences.
		{"SELECT s.NEXTVAL FROM dual", errNextval},
		{"select s.nextval from dual", errNextval},
		{`SELECT "HR"."S"."NEXTVAL" FROM dual`, errNextval},

		// A second statement.
		{"SELECT 1 FROM dual; DROP TABLE t", errSemicolon},
		{"SELECT 1 FROM dual;", errSemicolon},

		// What the lexer cannot read, it refuses.
		{"SELECT 'abc FROM dual", errUnreadable},
		{"SELECT 1 FROM dual /* not closed", errUnreadable},
		{`SELECT "abc FROM dual`, errUnreadable},
		{"SELECT q'[abc' FROM dual", errUnreadable},
		{"SELECT q' x ' FROM dual", errUnreadable},
		{"SELECT * FROM t FOR\u00a0UPDATE", errUnreadable},
		{"SELECT * FROM t FOR\u200bUPDATE", errUnreadable},
		{"SELECT 1 FROM dual\x00", errUnreadable},
	}
	for _, c := range cases {
		err := validateReadOnly(c.sql)
		if !errors.Is(err, c.want) {
			t.Errorf("validateReadOnly(%q) = %v, want %v", c.sql, err, c.want)
		}
	}
}
