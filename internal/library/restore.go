package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"
)

func readPath(tx *sql.Tx, r PathRow) (string, error) {
	var path string
	query := `SELECT path FROM ` + r.Table + ` WHERE site=? AND post_id=?`
	args := []any{r.Site, r.PostID}
	if r.Table != "downloads" {
		query += ` AND page_index=?`
		args = append(args, r.Page)
	}
	err := tx.QueryRow(query, args...).Scan(&path)
	return path, err
}
func restoreRows(tx *sql.Tx, original Plan) error {
	for _, r := range original.Snapshot.Paths {
		current, err := readPath(tx, r)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return err
		}
		if current != r.Expected {
			return fmt.Errorf("история уже изменена другой операцией: %s/%s", r.Site, r.PostID)
		}
		query := `UPDATE ` + r.Table + ` SET path=? WHERE site=? AND post_id=?`
		args := []any{r.Path, r.Site, r.PostID}
		if r.Table != "downloads" {
			query += ` AND page_index=?`
			args = append(args, r.Page)
		}
		if _, err = tx.Exec(query, args...); err != nil {
			return err
		}
	}
	for _, r := range original.Snapshot.Refs {
		expected := r.FileID
		for _, c := range original.Changes {
			if c.Before.ID == r.FileID && c.Keep != nil {
				expected = c.Keep.ID
				break
			}
		}
		var current, fp string
		if err := tx.QueryRow(`SELECT file_id,fingerprint FROM library_refs WHERE site=? AND post_id=? AND page_index=?`, r.Site, r.PostID, r.Page).Scan(&current, &fp); err != nil {
			return err
		}
		if current != expected || fp != r.Fingerprint {
			return fmt.Errorf("связь вложения изменена другой операцией")
		}
		if _, err := tx.Exec(`UPDATE library_refs SET file_id=?,relation=? WHERE site=? AND post_id=? AND page_index=?`, r.FileID, r.Relation, r.Site, r.PostID, r.Page); err != nil {
			return err
		}
	}
	if authorKind(original.Kind) {
		if err := checkGroups(tx, original.Groups, original.Layout); err != nil {
			return fmt.Errorf("группы авторов изменены после операции; восстановление отменено")
		}
		layout := ""
		if original.Kind == "layout" {
			layout = original.Snapshot.Layout
		}
		if err := writeGroups(tx, original.Groups, original.Snapshot.Groups, layout); err != nil {
			return err
		}
	}
	if original.Kind == "move" && original.Request.Remember {
		touched := map[string]Route{}
		for _, c := range original.Changes {
			r := Route{Root: c.Before.Root, Site: c.Before.Site, Author: c.Before.Author}
			touched[r.Root+"\x00"+r.Site+"\x00"+r.Author] = r
			for _, prev := range original.Snapshot.Routes {
				if prev.Root == r.Root && prev.TargetSite == r.Site && prev.TargetAuthor == r.Author {
					touched[prev.Root+"\x00"+prev.Site+"\x00"+prev.Author] = prev
				}
			}
		}
		for _, r := range touched {
			var site, author string
			err := tx.QueryRow(`SELECT target_site,target_author FROM library_routes WHERE root=? AND site=? AND author=?`, r.Root, r.Site, r.Author).Scan(&site, &author)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if err == nil && (site != original.Request.TargetSite || author != original.Request.TargetAuthor) {
				return fmt.Errorf("правило сохранения автора уже изменено")
			}
			if _, err = tx.Exec(`DELETE FROM library_routes WHERE root=? AND site=? AND author=?`, r.Root, r.Site, r.Author); err != nil {
				return err
			}
		}
		for _, r := range original.Snapshot.Routes {
			if _, ok := touched[r.Root+"\x00"+r.Site+"\x00"+r.Author]; ok {
				if _, err := tx.Exec(`INSERT INTO library_routes(root,site,author,target_site,target_author) VALUES(?,?,?,?,?)`, r.Root, r.Site, r.Author, r.TargetSite, r.TargetAuthor); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Restore reverses a completed plan only while all affected catalog revisions
// and source references still match it. It never overwrites a new file.
func (s *Service) Restore(ctx context.Context, id string) (err error) {
	if !s.work.TryLock() {
		return fmt.Errorf("операция библиотеки уже выполняется")
	}
	defer s.work.Unlock()
	defer s.invalidate()
	if err = s.checkRecovery(); err != nil {
		return err
	}
	original, err := s.Plan(id)
	if err != nil {
		return err
	}
	if original.State != "done" || original.Kind == "restore" {
		return fmt.Errorf("операцию нельзя восстановить")
	}
	release := func() {}
	if s.Begin != nil {
		release, err = s.Begin()
		if err != nil {
			return err
		}
	}
	defer release()
	p := Plan{ID: newID(), Kind: "restore", OriginalKind: original.Kind, State: "applying", Restores: id, Created: time.Now().Unix(), Changes: []Change{}}
	for _, c := range original.Changes {
		if err = validateDestination(c.Before.Root, c.Before.Path); err != nil {
			return err
		}
		if c.Backup != "" {
			if err = validateDestination(c.Before.Root, c.Backup); err != nil {
				return err
			}
		}
		current, e := s.File(c.After.ID)
		if e != nil {
			return e
		}
		if current.Version != c.After.Version || current.State != c.After.State || !same(current.Path, c.After.Path) {
			return fmt.Errorf("файл изменён последующей операцией; восстановление отменено")
		}
		if current.State == "active" {
			if err = validateFile(ctx, current); err != nil {
				return err
			}
		}
		restored := c.Before
		restored.Version = current.Version + 1
		restored.Origins = nil
		switch original.Kind {
		case "move":
			p.Steps = append(p.Steps, Step{c.After.Path, c.Before.Path, c.Before.SHA})
			p.RestoreFiles = append(p.RestoreFiles, restored)
		case "authors", "layout":
			if c.Backup != "" {
				p.Steps = append(p.Steps, Step{c.Backup, c.Before.Path, c.Before.SHA})
			} else if !same(c.After.Path, c.Before.Path) {
				p.Steps = append(p.Steps, Step{c.After.Path, c.Before.Path, c.Before.SHA})
			}
			p.RestoreFiles = append(p.RestoreFiles, restored)
		case "delete", "dedupe":
			p.Steps = append(p.Steps, Step{c.Backup, c.Before.Path, c.Before.SHA})
			p.RestoreFiles = append(p.RestoreFiles, restored)
		case "convert":
			p.Steps = append(p.Steps, Step{c.After.Path, quarantine(current, p.ID), c.After.SHA})
			if original.Request.Conversion.Replace {
				p.Steps = append(p.Steps, Step{c.Backup, c.Before.Path, c.Before.SHA})
				p.RestoreFiles = append(p.RestoreFiles, restored)
			} else {
				current.State = "quarantined"
				current.Version++
				p.RestoreFiles = append(p.RestoreFiles, current)
			}
		}
	}
	// Validate both the catalog and history before the first filesystem action.
	err = s.h.WithDB(func(db *sql.DB) error {
		tx, e := db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		for _, c := range original.Changes {
			if e = checkVersion(tx, c.After); e != nil {
				return e
			}
		}
		return restoreRows(tx, original)
	})
	if err != nil {
		return err
	}
	if err = s.storePlan(p); err != nil {
		return err
	}
	slog.Info("Библиотека: восстановление начато", "operation", p.ID, "restores", id)
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("внутренняя ошибка восстановления: %v", recovered)
		}
		if err == nil {
			return
		}
		e := rollbackSteps(p)
		p.State = "failed"
		p.Error = err.Error()
		if e != nil {
			p.State = "recovery_required"
			p.Error += "; " + e.Error()
		}
		_ = s.storePlan(p)
		slog.Error("Библиотека: восстановление не выполнено", "operation", p.ID, "error", p.Error)
	}()
	for i, step := range p.Steps {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		hash, e := hashFile(ctx, step.From)
		if e != nil {
			return e
		}
		if hash != step.SHA {
			return fmt.Errorf("файл восстановления изменён: %s", step.From)
		}
		p.Started = i + 1
		if err = s.storePlan(p); err != nil {
			return err
		}
		s.progress(Progress{Stage: "Восстановление", Done: i, Total: len(p.Steps), Path: step.From})
		if err = moveChecked(ctx, step.From, step.To, step.SHA); err != nil {
			p.Started = i
			return err
		}
	}
	for i := range p.RestoreFiles {
		f := &p.RestoreFiles[i]
		if f.State == "active" {
			st, e := os.Stat(f.Path)
			if e != nil {
				return e
			}
			f.Modified = st.ModTime().UnixNano()
			f.Size = st.Size()
		}
	}
	err = s.h.WithDB(func(db *sql.DB) error {
		tx, e := db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		for _, c := range original.Changes {
			if e = checkVersion(tx, c.After); e != nil {
				return e
			}
		}
		if e = restoreRows(tx, original); e != nil {
			return e
		}
		for _, f := range p.RestoreFiles {
			if e = saveFile(tx, f); e != nil {
				return e
			}
		}
		original.State = "restored"
		data, _ := json.Marshal(original)
		if _, e = tx.Exec(`UPDATE library_operations SET state='restored',data=? WHERE id=?`, string(data), original.ID); e != nil {
			return e
		}
		p.State = "done"
		data, _ = json.Marshal(p)
		if _, e = tx.Exec(`UPDATE library_operations SET state='done',data=? WHERE id=?`, string(data), p.ID); e != nil {
			return e
		}
		return tx.Commit()
	})
	if err != nil {
		return err
	}
	changes := map[string]string{}
	for _, c := range original.Changes {
		if original.Kind == "move" || original.Kind == "convert" || authorKind(original.Kind) && c.Backup == "" {
			changes[c.After.Path] = c.Before.Path
		}
	}
	slog.Info("Библиотека: восстановление завершено", "operation", p.ID, "restores", id)
	if s.Changed != nil {
		s.Changed(changes)
	}
	return nil
}
