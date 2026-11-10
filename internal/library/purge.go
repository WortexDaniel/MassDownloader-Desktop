package library

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

func (p Plan) HasQuarantine() bool {
	for _, step := range p.Steps {
		if strings.Contains(filepath.ToSlash(step.To), "/.massdownloader-trash/") {
			return true
		}
	}
	return false
}

// Purge only touches the private backups belonging to an explicitly chosen
// journal operation. Active files and source/favourite records are retained.
func (s *Service) Purge(ctx context.Context, id string) error {
	if !s.work.TryLock() {
		return fmt.Errorf("операция библиотеки уже выполняется")
	}
	defer s.work.Unlock()
	defer s.invalidate()
	p, err := s.Plan(id)
	if err != nil {
		return err
	}
	if p.State != "done" && p.State != "purging" {
		return fmt.Errorf("карантин этой операции недоступен")
	}
	backups := []Step{}
	for _, step := range p.Steps {
		if strings.Contains(filepath.ToSlash(step.To), "/.massdownloader-trash/") {
			backups = append(backups, step)
		}
	}
	if len(backups) == 0 {
		return fmt.Errorf("у операции нет файлов в карантине")
	}
	p.State = "purging"
	if err = s.storePlan(p); err != nil {
		return err
	}
	slog.Info("Библиотека: очистка карантина начата", "operation", id, "files", len(backups))
	for i, step := range backups {
		if err = validateJournalPath(p, step.To); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err = os.Stat(step.To); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		hash, e := hashFile(ctx, step.To)
		if e != nil {
			return e
		}
		if hash != step.SHA {
			return fmt.Errorf("файл карантина изменён: %s", step.To)
		}
		s.progress(Progress{Stage: "Очистка карантина", Done: i, Total: len(backups), Path: step.To})
		if err = os.Remove(step.To); err != nil {
			return err
		}
		slog.Debug("Библиотека: файл карантина удалён", "operation", id, "path", step.To)
	}
	err = s.h.WithDB(func(db *sql.DB) error {
		tx, e := db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		for _, c := range p.Changes {
			if c.Backup != "" && c.After.State == "quarantined" {
				if _, e = tx.Exec(`UPDATE library_files SET state='deleted',data=json_set(data,'$.state','deleted') WHERE id=? AND state='quarantined' AND json_extract(data,'$.version')=?`, c.After.ID, c.After.Version); e != nil {
					return e
				}
			}
		}
		if _, e = tx.Exec(`UPDATE library_operations SET state='purged',data=json_set(data,'$.state','purged') WHERE id=?`, p.ID); e != nil {
			return e
		}
		return tx.Commit()
	})
	if err != nil {
		return err
	}
	slog.Info("Библиотека: карантин очищен", "operation", id)
	if s.Changed != nil {
		s.Changed(nil)
	}
	return nil
}
