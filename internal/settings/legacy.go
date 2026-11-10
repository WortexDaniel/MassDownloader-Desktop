// Legacy history.db merge (settings.py::_migrate_legacy, history branch).
package settings

import (
	"database/sql"
	"log"

	_ "modernc.org/sqlite"
)

func mergeHistoryDB(srcPath, dstPath string) {
	dst, err := sql.Open("sqlite", "file:"+filepathToSlash(dstPath)+
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return
	}
	defer dst.Close()
	if _, err := dst.Exec(`CREATE TABLE IF NOT EXISTS downloads (
		md5 TEXT NOT NULL, site TEXT NOT NULL,
		post_id INTEGER NOT NULL, path TEXT NOT NULL,
		ts INTEGER NOT NULL, PRIMARY KEY (site, post_id))`); err != nil {
		return
	}
	src, err := sql.Open("sqlite", "file:"+filepathToSlash(srcPath)+"?mode=ro")
	if err != nil {
		return
	}
	defer src.Close()
	rows, err := src.Query(`SELECT md5, site, post_id, path, ts FROM downloads`)
	if err != nil {
		return
	}
	defer rows.Close()
	tx, err := dst.Begin()
	if err != nil {
		return
	}
	n := 0
	for rows.Next() {
		var md5Sum, site, path string
		var pid, ts int64
		if err := rows.Scan(&md5Sum, &site, &pid, &path, &ts); err != nil {
			continue
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO downloads VALUES (?, ?, ?, ?, ?)`,
			md5Sum, site, pid, path, ts); err != nil {
			continue
		}
		n++
	}
	rows.Close()
	if err := tx.Commit(); err != nil {
		log.Printf("legacy history merge: %v", err)
	} else if n > 0 {
		log.Printf("migrated %d history rows from %s", n, srcPath)
	}
}

func filepathToSlash(p string) string {
	out := make([]byte, len(p))
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' {
			out[i] = '/'
		} else {
			out[i] = p[i]
		}
	}
	return string(out)
}
