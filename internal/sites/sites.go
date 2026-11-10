// Package sites wires every engine package's Register into one import so the
// binary populates the registry at startup (mirrors app/sites/__init__.py).
package sites

import (
	"massdownloader/internal/core"
	"massdownloader/internal/sites/danbooru"
	"massdownloader/internal/sites/deviantart"
	"massdownloader/internal/sites/e621"
	"massdownloader/internal/sites/furaffinity"
	"massdownloader/internal/sites/gelbooru"
	"massdownloader/internal/sites/kemono"
	"massdownloader/internal/sites/misskey"
	"massdownloader/internal/sites/moebooru"
	"massdownloader/internal/sites/onlyhaven"
	"massdownloader/internal/sites/pixiv"
	"massdownloader/internal/sites/sankaku"
	"massdownloader/internal/sites/twitter"
)

// Register installs all engines and sites in the README's registration order:
// e621/e6ai; Danbooru/AIBooru; Konachan; Gelbooru family; Sankaku; Kemono;
// X; FurAffinity; Pixiv; DeviantArt; Misskey.io — 18 sites, 14 engines.
func Register() {
	e621.Register()     // e621, e6ai
	danbooru.Register() // danbooru, aibooru
	moebooru.Register() // konachan
	gelbooru.Register() // rule34, xbooru, tbib, gelbooru.com
	sankaku.Register()  // sankaku (channel), idolcomplex
	kemono.Register()   // kemono.cr, coomer.st
	onlyhaven.Register()
	twitter.Register() // x.com
	furaffinity.Register()
	pixiv.Register()
	deviantart.Register()
	misskey.Register() // misskey.io
}

// Counts returns registered engine/site totals (a startup self-check).
func Counts() (engines, siteCount int) {
	return core.EngineCount(), len(core.SiteKeys())
}
