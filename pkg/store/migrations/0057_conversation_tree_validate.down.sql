-- Nothing to do. A validated constraint cannot be un-validated: there is no
-- ALTER TABLE … UNVALIDATE CONSTRAINT. The only reversal is dropping the
-- constraints, which 0056's down migration already does, so repeating it here
-- would make this file a second, silently divergent copy of that DDL.
SELECT 1;
