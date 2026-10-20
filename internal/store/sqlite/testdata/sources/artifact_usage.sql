SELECT COALESCE(SUM(a.size_bytes), 0) FROM artifacts a
JOIN runs r ON r.id = a.run_id
WHERE r.repo_id = ? AND a.finalized = 1