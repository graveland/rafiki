-- 0068 converts the three Timescale hypertables — conversations.conversation_turn,
-- openrouter.broadcast and conversations.raw_http_request — into ordinary tables.
-- It is a no-op where timescaledb is not installed, and where a table is already
-- ordinary. It never touches the extension itself. Each hypertable is copied into
-- a plain twin and the twin is verified before the original is dropped: same row
-- count, and the same count of the nullable columns named below. A copy that lost
-- anything aborts the migration, and any error at all rolls the whole migration
-- back, leaving every table exactly as it was.

-- Is a table a Timescale hypertable? The catalog is reached only through EXECUTE,
-- so a server without timescaledb never plans a reference to its schema.
CREATE FUNCTION pg_temp.rafiki_is_hypertable(sch text, tbl text) RETURNS boolean
LANGUAGE plpgsql AS $fn$
DECLARE
	r boolean;
BEGIN
	EXECUTE 'SELECT EXISTS (SELECT 1 FROM timescaledb_information.hypertables
	                         WHERE hypertable_schema = $1 AND hypertable_name = $2)'
	   INTO r USING sch, tbl;
	RETURN r;
END
$fn$;

-- Re-applies a captured ACL verbatim. GRANT ... ON TABLE is valid on a view too.
CREATE FUNCTION pg_temp.rafiki_regrant(target text, acl aclitem[]) RETURNS void
LANGUAGE plpgsql AS $fn$
DECLARE
	r record;
BEGIN
	IF acl IS NULL THEN
		RETURN;
	END IF;
	FOR r IN SELECT * FROM aclexplode(acl) LOOP
		EXECUTE format('GRANT %s ON TABLE %s TO %s%s',
			r.privilege_type,
			target,
			CASE WHEN r.grantee = 0 THEN 'PUBLIC' ELSE quote_ident(pg_get_userbyid(r.grantee)) END,
			CASE WHEN r.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END);
	END LOOP;
END
$fn$;

-- Rewrites one hypertable as an ordinary table, preserving its rows, owner, ACL,
-- comment, constraints, indexes and identity sequence state. Names are carried as
-- TEXT throughout: a regclass of a dropped table prints as a bare oid.
CREATE FUNCTION pg_temp.rafiki_unhypertable(sch text, tbl text, counted text[]) RETURNS void
LANGUAGE plpgsql AS $fn$
DECLARE
	qname        text := format('%I.%I', sch, tbl);
	qnew         text := format('%I.%I', sch, tbl || '_plain');
	owner        text;
	acl          aclitem[];
	comment      text;
	cons         text[];
	idxs         text[];
	post         text[] := '{}';
	col          text;
	stmt         text;
	att          record;
	old_seq      text;
	old_seq_name text;
	new_seq      text;
	last_value   bigint;
	is_called    boolean;
	old_n        bigint;
	new_n        bigint;
	old_c        bigint;
	new_c        bigint;
BEGIN
	IF NOT pg_temp.rafiki_is_hypertable(sch, tbl) THEN
		RETURN;
	END IF;

	EXECUTE format('LOCK TABLE %s IN ACCESS EXCLUSIVE MODE', qname);

	SELECT pg_get_userbyid(c.relowner), c.relacl, obj_description(c.oid, 'pg_class')
	  INTO owner, acl, comment
	  FROM pg_class c
	 WHERE c.oid = qname::regclass;

	-- The primary key first: everything else references it.
	SELECT coalesce(array_agg(format('ALTER TABLE %s ADD CONSTRAINT %I %s', qname, k.conname, pg_get_constraintdef(k.oid))
	                          ORDER BY CASE k.contype WHEN 'p' THEN 0 WHEN 'u' THEN 1 ELSE 2 END, k.conname), '{}')
	  INTO cons
	  FROM pg_constraint k
	 WHERE k.conrelid = qname::regclass
	   AND k.contype IN ('p', 'u', 'c', 'x', 'f');

	SELECT coalesce(array_agg(pg_get_indexdef(i.indexrelid) ORDER BY i.indexrelid), '{}')
	  INTO idxs
	  FROM pg_index i
	 WHERE i.indrelid = qname::regclass
	   AND NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conindid = i.indexrelid);

	EXECUTE format('CREATE TABLE %s (LIKE %s INCLUDING DEFAULTS INCLUDING IDENTITY INCLUDING STORAGE INCLUDING COMMENTS INCLUDING STATISTICS)', qnew, qname);

	-- An identity column's sequence is dropped with the old table, so after the
	-- swap the twin's own sequence takes the old sequence's name and state.
	FOR att IN
		SELECT a.attname
		  FROM pg_attribute a
		 WHERE a.attrelid = qname::regclass
		   AND a.attnum > 0
		   AND NOT a.attisdropped
		   AND a.attidentity <> ''
		 ORDER BY a.attnum
	LOOP
		old_seq := pg_get_serial_sequence(qname, att.attname);
		SELECT c.relname INTO old_seq_name FROM pg_class c WHERE c.oid = old_seq::regclass;
		EXECUTE format('SELECT last_value, is_called FROM %s', old_seq) INTO last_value, is_called;
		new_seq := pg_get_serial_sequence(qnew, att.attname);
		post := post || format('ALTER SEQUENCE %s RENAME TO %I', new_seq, old_seq_name);
		post := post || format('SELECT setval(%L, %s, %s)', old_seq, last_value,
			CASE WHEN is_called THEN 'true' ELSE 'false' END);
	END LOOP;

	EXECUTE format('INSERT INTO %s OVERRIDING SYSTEM VALUE SELECT * FROM %s', qnew, qname);

	EXECUTE format('SELECT count(*) FROM %s', qname) INTO old_n;
	EXECUTE format('SELECT count(*) FROM %s', qnew) INTO new_n;
	IF old_n <> new_n THEN
		RAISE EXCEPTION 'rafiki 0068: copying % into % lost rows (% in the original, % in the copy); nothing was changed', qname, qnew, old_n, new_n;
	END IF;
	FOREACH col IN ARRAY counted LOOP
		EXECUTE format('SELECT count(%I) FROM %s', col, qname) INTO old_c;
		EXECUTE format('SELECT count(%I) FROM %s', col, qnew) INTO new_c;
		IF old_c <> new_c THEN
			RAISE EXCEPTION 'rafiki 0068: copying % lost column % values (% in the original, % in the copy); nothing was changed', qname, col, old_c, new_c;
		END IF;
	END LOOP;

	-- No CASCADE: a dependent object this migration does not know about fails the
	-- migration loudly instead of being dropped with the hypertable.
	EXECUTE format('DROP TABLE %s', qname);
	EXECUTE format('ALTER TABLE %s RENAME TO %I', qnew, tbl);

	FOREACH stmt IN ARRAY post || cons || idxs LOOP
		EXECUTE stmt;
	END LOOP;

	EXECUTE format('ALTER TABLE %s OWNER TO %I', qname, owner);
	PERFORM pg_temp.rafiki_regrant(qname, acl);
	IF comment IS NOT NULL THEN
		EXECUTE format('COMMENT ON TABLE %s IS %L', qname, comment);
	END IF;
	EXECUTE format('ANALYZE %s', qname);
END
$fn$;

DO $do$
DECLARE
	viewdef  text;
	vowner   text;
	vacl     aclitem[];
	vopts    text;
	vcomment text;
BEGIN
	IF NOT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
		RETURN;
	END IF;

	-- A compressed chunk's rows are only visible to an ordinary scan while the
	-- columnar scan path is enabled, and an operator can turn it off at DATABASE
	-- level, which this migration's own session then inherits. The copy and the
	-- count checks below both read through that path, so with it off a compressed
	-- chunk looks empty: the copy is short, the counts agree, and DROP TABLE
	-- destroys the rows. Force it on for this transaction only. A TimescaleDB
	-- without the parameter leaves it unset and migrates as before.
	IF EXISTS (SELECT 1 FROM pg_settings WHERE name = 'timescaledb.enable_columnarscan') THEN
		PERFORM set_config('timescaledb.enable_columnarscan', 'on', true);
	END IF;

	PERFORM pg_temp.rafiki_unhypertable('conversations', 'raw_http_request', '{}');
	PERFORM pg_temp.rafiki_unhypertable('openrouter', 'broadcast', '{}');

	IF pg_temp.rafiki_is_hypertable('conversations', 'conversation_turn') THEN
		-- v_turn is the hypertable's only dependent, and it is the Grafana FDW
		-- surface, so its grants matter: capture what is deployed, drop it for the
		-- swap, and rebuild it from the capture rather than from a migration's text.
		SELECT pg_get_viewdef(c.oid, true), pg_get_userbyid(c.relowner), c.relacl,
		       array_to_string(c.reloptions, ', '), obj_description(c.oid, 'pg_class')
		  INTO viewdef, vowner, vacl, vopts, vcomment
		  FROM pg_class c
		 WHERE c.oid = 'conversations.v_turn'::regclass;

		DROP VIEW conversations.v_turn;

		PERFORM pg_temp.rafiki_unhypertable('conversations', 'conversation_turn',
		                                    ARRAY['response', 'request_head_id']);

		EXECUTE format('CREATE VIEW conversations.v_turn %s AS %s',
			CASE WHEN coalesce(vopts, '') <> '' THEN 'WITH (' || vopts || ')' ELSE '' END,
			viewdef);
		EXECUTE format('ALTER VIEW conversations.v_turn OWNER TO %I', vowner);
		PERFORM pg_temp.rafiki_regrant('conversations.v_turn', vacl);
		IF vcomment IS NOT NULL THEN
			EXECUTE format('COMMENT ON VIEW conversations.v_turn IS %L', vcomment);
		END IF;
	END IF;
END
$do$;

DROP FUNCTION pg_temp.rafiki_unhypertable(text, text, text[]);
DROP FUNCTION pg_temp.rafiki_regrant(text, aclitem[]);
DROP FUNCTION pg_temp.rafiki_is_hypertable(text, text);
