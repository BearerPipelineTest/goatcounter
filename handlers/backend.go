// Copyright © 2019 Martin Tournoij <martin@arp242.net>
// This file is part of GoatCounter and published under the terms of the EUPL
// v1.2, which can be found in the LICENSE file or at http://eupl12.zgo.at

package handlers

import (
	"compress/gzip"
	"context"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/arp242/geoip2-golang"
	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/monoculum/formam"
	"zgo.at/blackmail"
	"zgo.at/errors"
	"zgo.at/goatcounter"
	"zgo.at/goatcounter/acme"
	"zgo.at/goatcounter/bgrun"
	"zgo.at/goatcounter/cfg"
	"zgo.at/goatcounter/pack"
	"zgo.at/guru"
	"zgo.at/isbot"
	"zgo.at/tz"
	"zgo.at/zdb"
	"zgo.at/zhttp"
	"zgo.at/zhttp/header"
	"zgo.at/zlog"
	"zgo.at/zstd/zjson"
	"zgo.at/zstd/zstring"
	"zgo.at/zstd/zsync"
	"zgo.at/zstripe"
	"zgo.at/zvalidate"
)

type backend struct{}

// DailyView forces the "view by day" if the number of selected days is larger than this.
const DailyView = 90

func (h backend) Mount(r chi.Router, db zdb.DB) {
	if !cfg.Prod {
		r.Use(delay())
	}

	r.Use(
		zhttp.RealIP,
		zhttp.Unpanic(cfg.Prod),
		addctx(db, true),
		middleware.RedirectSlashes,
		zhttp.NoStore,
		zhttp.WrapWriter)

	api{}.mount(r, db)

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		zhttp.ErrPage(w, r, 404, errors.New("Not Found"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		zhttp.ErrPage(w, r, 405, errors.New("Method Not Allowed"))
	})
	r.Get("/status", zhttp.Wrap(h.status()))

	{
		rr := r.With(zhttp.Headers(nil))
		rr.Get("/robots.txt", zhttp.HandlerRobots([][]string{{"User-agent: *", "Disallow: /"}}))
		rr.Post("/jserr", zhttp.HandlerJSErr())
		rr.Post("/csp", zhttp.HandlerCSP())

		// 4 pageviews/second should be more than enough.
		rateLimited := rr.With(zhttp.Ratelimit(zhttp.RatelimitOptions{
			Client: func(r *http.Request) string {
				// Add in the User-Agent to reduce the problem of multiple
				// people in the same building hitting the limit.
				return r.RemoteAddr + r.UserAgent()
			},
			Store: zhttp.NewRatelimitMemory(),
			Limit: func(r *http.Request) (int, int64) {
				if !cfg.Prod {
					return 1 << 30, 1
				}
				// From httpbuf
				// TODO: in some setups this may always be true, e.g. when proxy
				// through nginx without settings this properly. Need to check.
				if r.RemoteAddr == "127.0.0.1" {
					return 1 << 14, 1
				}
				return 4, 1
			},
		}))
		countHandler := zhttp.Wrap(h.count)
		rateLimited.Get("/count", countHandler)
		rateLimited.Post("/count", countHandler) // to support navigator.sendBeacon (JS)
	}

	{
		headers := http.Header{
			"Strict-Transport-Security": []string{"max-age=2592000"},
			"X-Frame-Options":           []string{"deny"},
			"X-Content-Type-Options":    []string{"nosniff"},
		}
		// https://stripe.com/docs/security#content-security-policy
		ds := []string{""}
		if cfg.DomainStatic == "" {
			ds[0] = header.CSPSourceSelf
		} else {
			ds[0] = cfg.DomainStatic
		}
		gc := "https://gc.goatcounter.com"
		if !cfg.Prod {
			gc = "http://gc." + cfg.Domain
		}
		header.SetCSP(headers, header.CSPArgs{
			header.CSPDefaultSrc: {header.CSPSourceNone},
			header.CSPImgSrc:     append(ds, "data:", gc),
			header.CSPScriptSrc: append(ds, "https://chat.goatcounter.com", "https://js.stripe.com",
				// Inline GoatCounter setup
				"https://gc.zgo.at", "'sha256-rhp1kopsm+UqtrN5qCeSn81YXeO4wJtXDvQE00OrLoQ='"),
			header.CSPStyleSrc:    append(ds, header.CSPSourceUnsafeInline), // style="height: " on the charts.
			header.CSPFontSrc:     ds,
			header.CSPFormAction:  {header.CSPSourceSelf},
			header.CSPConnectSrc:  {header.CSPSourceSelf, "https://chat.goatcounter.com", "https://api.stripe.com"},
			header.CSPFrameSrc:    {"https://js.stripe.com", "https://hooks.stripe.com"},
			header.CSPManifestSrc: ds,
			// Too much noise: header.CSPReportURI:  {"/csp"},
		})

		a := r.With(zhttp.Headers(headers), keyAuth)
		if !cfg.Prod {
			a = a.With(zhttp.Log(true, ""))
		}

		user{}.mount(a)
		{
			ap := a.With(loggedInOrPublic)
			ap.Get("/", zhttp.Wrap(h.dashboard))
			ap.Get("/pages", zhttp.Wrap(h.pages))
			ap.Get("/hchart-detail", zhttp.Wrap(h.hchartDetail))
			ap.Get("/hchart-more", zhttp.Wrap(h.hchartMore))
		}
		{
			af := a.With(loggedIn)
			if zstripe.SecretKey != "" && zstripe.SignSecret != "" && zstripe.PublicKey != "" {
				billing{}.mount(a, af)
			}
			af.Get("/updates", zhttp.Wrap(h.updates))
			af.Get("/settings", zhttp.Wrap(h.settings))
			af.Get("/code", zhttp.Wrap(h.code))
			af.Get("/ip", zhttp.Wrap(h.ip))
			af.Post("/save-settings", zhttp.Wrap(h.saveSettings))
			af.With(zhttp.Ratelimit(zhttp.RatelimitOptions{
				Client:  zhttp.RatelimitIP,
				Store:   zhttp.NewRatelimitMemory(),
				Limit:   zhttp.RatelimitLimit(1, 3600),
				Message: "you can request only one export per hour",
			})).Post("/export", zhttp.Wrap(h.startExport))
			af.Get("/export/{id}", zhttp.Wrap(h.downloadExport))
			af.Post("/import", zhttp.Wrap(h.importFile))
			af.Post("/add", zhttp.Wrap(h.addSubsite))
			af.Get("/remove/{id}", zhttp.Wrap(h.removeSubsiteConfirm))
			af.Post("/remove/{id}", zhttp.Wrap(h.removeSubsite))
			af.Get("/purge", zhttp.Wrap(h.purgeConfirm))
			af.Post("/purge", zhttp.Wrap(h.purge))
			af.Post("/delete", zhttp.Wrap(h.delete))
			admin{}.mount(af)
		}
	}
}

// Use GIF because it's the smallest filesize (PNG is 116 bytes, vs 43 for GIF).
var gif = []byte{0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x1, 0x0, 0x1, 0x0, 0x80,
	0x1, 0x0, 0x0, 0x0, 0x0, 0xff, 0xff, 0xff, 0x21, 0xf9, 0x4, 0x1, 0xa, 0x0,
	0x1, 0x0, 0x2c, 0x0, 0x0, 0x0, 0x0, 0x1, 0x0, 0x1, 0x0, 0x0, 0x2, 0x2, 0x4c,
	0x1, 0x0, 0x3b}

var geodb = func() *geoip2.Reader {
	g, err := geoip2.FromBytes(pack.GeoDB)
	if err != nil {
		panic(err)
	}
	return g
}()

func geo(ip string) string {
	loc, _ := geodb.Country(net.ParseIP(ip))
	return loc.Country.IsoCode
}

func (h backend) status() func(w http.ResponseWriter, r *http.Request) error {
	started := goatcounter.Now()
	return func(w http.ResponseWriter, r *http.Request) error {
		return zhttp.JSON(w, map[string]string{
			"uptime":  goatcounter.Now().Sub(started).String(),
			"version": cfg.Version,
		})
	}
}

func (h backend) count(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "image/gif")

	bot := isbot.Bot(r)

	// Don't track pages fetched with the browser's prefetch algorithm.
	if bot == isbot.BotPrefetch {
		return zhttp.Bytes(w, gif)
	}

	site := goatcounter.MustGetSite(r.Context())
	for _, ip := range site.Settings.IgnoreIPs {
		if ip == r.RemoteAddr {
			w.Header().Add("X-Goatcounter", fmt.Sprintf("ignored because %q is in the IP ignore list", ip))
			w.WriteHeader(http.StatusAccepted)
			return zhttp.Bytes(w, gif)
		}
	}

	hit := goatcounter.Hit{
		Site:      site.ID,
		Browser:   r.UserAgent(),
		Location:  geo(r.RemoteAddr),
		CreatedAt: goatcounter.Now(),
	}

	err := formam.NewDecoder(&formam.DecoderOptions{TagName: "json"}).Decode(r.URL.Query(), &hit)
	if err != nil {
		w.Header().Add("X-Goatcounter", fmt.Sprintf("error decoding parameters: %s", err))
		w.WriteHeader(400)
		return zhttp.Bytes(w, gif)
	}
	if hit.Bot > 0 && hit.Bot < 150 {
		w.Header().Add("X-Goatcounter", fmt.Sprintf("wrong value: b=%d", hit.Bot))
		w.WriteHeader(400)
		return zhttp.Bytes(w, gif)
	}

	if isbot.Is(bot) { // Prefer the backend detection.
		hit.Bot = int(bot)
	}

	if uint8(hit.Bot) >= isbot.BotJSPhanton {
		ctx := zdb.With(context.Background(), zdb.MustGet(r.Context()))
		bgrun.Run(func() {
			bl := goatcounter.AdminBotlog{
				Bot:       hit.Bot,
				UserAgent: r.UserAgent(),
				Headers:   r.Header,
				URL:       r.RequestURI,
			}
			err := bl.Insert(ctx, r.RemoteAddr)
			if err != nil {
				zlog.Error(err)
			}
		})
	}

	// TODO: move to memstore?
	{
		var sess goatcounter.Session
		first, err := sess.GetOrCreate(r.Context(), hit.Path, r.UserAgent(), r.RemoteAddr)
		if err != nil {
			zlog.Error(err)
		}

		hit.Session = &sess.ID
		if first {
			hit.FirstVisit = zdb.Bool(true)
		}
	}

	err = hit.Validate(r.Context())
	if err != nil {
		w.Header().Add("X-Goatcounter", fmt.Sprintf("not valid: %s", err))
		w.WriteHeader(400)
		return zhttp.Bytes(w, gif)
	}

	goatcounter.Memstore.Append(hit)
	return zhttp.Bytes(w, gif)
}

const day = 24 * time.Hour

func (h backend) dashboard(w http.ResponseWriter, r *http.Request) error {
	site := goatcounter.MustGetSite(r.Context())

	// Cache much more aggressively for public displays. Don't care so much if
	// it's outdated by an hour.
	if site.Settings.Public && goatcounter.GetUser(r.Context()).ID == 0 {
		w.Header().Set("Cache-Control", "public,max-age=3600")
		w.Header().Set("Vary", "Cookie")
	}

	start, end, err := getPeriod(w, r, site)
	if err != nil {
		zhttp.FlashError(w, err.Error())
	}
	if start.IsZero() || end.IsZero() {
		y, m, d := goatcounter.Now().In(site.Settings.Timezone.Loc()).Date()
		now := time.Date(y, m, d, 0, 0, 0, 0, site.Settings.Timezone.Loc())
		start = now.Add(-7 * day).UTC()
		end = time.Date(y, m, d, 23, 59, 59, 9, now.Location()).UTC().Round(time.Second)
	}

	showRefs := r.URL.Query().Get("showrefs")
	filter := r.URL.Query().Get("filter")
	daily, forcedDaily := getDaily(r, start, end)

	// l := zlog.Module("dashboard").Field("site", site.ID)

	type Widget struct {
		Name string
		Type string // "full-width", "hchart"
		HTML template.HTML
	}
	var widgets []Widget
	wantWidgets := []string{"totals",
		"pages", "totalpages", "toprefs", "browsers", "systems", "sizes", "locations"}

	if showRefs == "" {
		wantWidgets = append(wantWidgets, "refs")
	}
	if zstring.Contains(wantWidgets, "pages") {
		wantWidgets = append(wantWidgets, "max")
	}

	var (
		wg    sync.WaitGroup
		bgErr = errors.NewGroup(20)
	)

	var data struct {
		total, totalUnique int

		pages struct {
			display, uniqueDisplay int
			max                    int
			more                   bool
			pages                  goatcounter.HitStats
			refs                   goatcounter.Stats
		}

		totalPages struct {
			max   int
			total goatcounter.HitStat
		}

		topRefs  goatcounter.Stats
		browsers goatcounter.Stats
		systems  goatcounter.Stats
		sizeStat goatcounter.Stats
		locStat  goatcounter.Stats
	}

	funcs := map[string]func() error{
		"totals": func() (err error) {
			data.total, data.totalUnique, err = goatcounter.GetTotalCount(r.Context(), start, end, filter)
			return err
		},

		"pages": func() (err error) {
			data.pages.display, data.pages.uniqueDisplay, data.pages.more, err = data.pages.pages.List(
				r.Context(), start, end, filter, nil, daily)
			return err
		},
		"refs": func() (err error) {
			return data.pages.refs.ListRefsByPath(r.Context(), showRefs, start, end, 0)
		},

		"max": func() (err error) {
			data.pages.max, err = goatcounter.GetMax(r.Context(), start, end, filter, daily)
			return err
		},

		"totalpages": func() (err error) {
			data.totalPages.max, err = data.totalPages.total.Totals(r.Context(), start, end, filter, daily)
			return err
		},

		"toprefs":   func() (err error) { return data.topRefs.ListTopRefs(r.Context(), start, end, 0) },
		"browsers":  func() (err error) { return data.browsers.ListBrowsers(r.Context(), start, end, 6, 0) },
		"systems":   func() (err error) { return data.systems.ListSystems(r.Context(), start, end, 6, 0) },
		"sizes":     func() (err error) { return data.sizeStat.ListSizes(r.Context(), start, end) },
		"locations": func() (err error) { return data.locStat.ListLocations(r.Context(), start, end, 6, 0) },
	}

	for _, w := range wantWidgets {
		wg.Add(1)
		go func(w string) {
			defer zlog.Recover(func(l zlog.Log) zlog.Log { return l.Field("data widget", w).FieldsRequest(r) })
			defer wg.Done()

			l := zlog.Module("dashboard")
			bgErr.Append(funcs[w]())
			l.Since(w)
		}(w)
	}

	subs, err := site.ListSubs(r.Context())
	if err != nil {
		return err
	}

	cd := cfg.DomainCount
	if cd == "" {
		cd = goatcounter.MustGetSite(r.Context()).Domain()
		if cfg.Port != "" {
			cd += ":" + cfg.Port
		}
	}

	zsync.Wait(r.Context(), &wg)
	if bgErr.Len() > 0 {
		return bgErr
	}

	render := map[string]func() (Widget, error){
		"pages": func() (Widget, error) {
			tpl, err := zhttp.ExecuteTpl("_dashboard_pages.gohtml", struct {
				Context     context.Context
				Pages       goatcounter.HitStats
				Site        *goatcounter.Site
				PeriodStart time.Time
				PeriodEnd   time.Time
				Daily       bool
				ForcedDaily bool
				Max         int

				TotalDisplay       int
				TotalUniqueDisplay int

				TotalHits       int
				TotalUniqueHits int
				MorePages       bool

				Refs     goatcounter.Stats
				ShowRefs string
			}{r.Context(), data.pages.pages, site, start, end, daily, forcedDaily, data.pages.max,
				data.pages.display, data.pages.uniqueDisplay,
				data.total, data.totalUnique, data.pages.more,
				data.pages.refs, showRefs})

			return Widget{Type: "full-width", HTML: template.HTML(tpl)}, err
		},

		"totalpages": func() (Widget, error) {
			tpl, err := zhttp.ExecuteTpl("_dashboard_totals.gohtml", struct {
				Context         context.Context
				Site            *goatcounter.Site
				Page            goatcounter.HitStat
				Daily           bool
				Max             int
				TotalHits       int
				TotalUniqueHits int
			}{r.Context(), site, data.totalPages.total, daily, data.totalPages.max,
				data.total, data.totalUnique})
			return Widget{Type: "full-width", HTML: template.HTML(tpl)}, err
		},

		"toprefs": func() (Widget, error) {
			tpl, err := zhttp.ExecuteTpl("_dashboard_toprefs.gohtml", struct {
				Context         context.Context
				TotalUniqueHits int
				Stats           goatcounter.Stats
			}{r.Context(), data.totalUnique, data.topRefs})
			return Widget{Type: "hchart", HTML: template.HTML(tpl)}, err
		},
		"browsers": func() (Widget, error) {
			tpl, err := zhttp.ExecuteTpl("_dashboard_browsers.gohtml", struct {
				Context         context.Context
				TotalUniqueHits int
				Stats           goatcounter.Stats
			}{r.Context(), data.totalUnique, data.browsers})
			return Widget{Type: "hchart", HTML: template.HTML(tpl)}, err
		},
		"systems": func() (Widget, error) {
			tpl, err := zhttp.ExecuteTpl("_dashboard_systems.gohtml", struct {
				Context         context.Context
				TotalUniqueHits int
				Stats           goatcounter.Stats
			}{r.Context(), data.totalUnique, data.systems})
			return Widget{Type: "hchart", HTML: template.HTML(tpl)}, err
		},
		"sizes": func() (Widget, error) {
			tpl, err := zhttp.ExecuteTpl("_dashboard_sizes.gohtml", struct {
				Context         context.Context
				TotalUniqueHits int
				Stats           goatcounter.Stats
			}{r.Context(), data.totalUnique, data.sizeStat})
			return Widget{Type: "hchart", HTML: template.HTML(tpl)}, err
		},
		"locations": func() (Widget, error) {
			tpl, err := zhttp.ExecuteTpl("_dashboard_locations.gohtml", struct {
				Context         context.Context
				TotalUniqueHits int
				Stats           goatcounter.Stats
			}{r.Context(), data.totalUnique, data.locStat})
			return Widget{Type: "hchart", HTML: template.HTML(tpl)}, err
		},
	}
	for _, w := range wantWidgets {
		wg.Add(1)
		go func(w string) {
			defer zlog.Recover(func(l zlog.Log) zlog.Log { return l.Field("tpl widget", w).FieldsRequest(r) })
			defer wg.Done()

			f, ok := render[w]
			if !ok {
				return
			}

			wid, err := f()
			wid.Name = w
			widgets = append(widgets, wid)
			bgErr.Append(err)
		}(w)
	}

	zsync.Wait(r.Context(), &wg)
	if bgErr.Len() > 0 {
		return bgErr
	}

	// Ensure correct order.
	sortmap := make(map[string]int, len(wantWidgets))
	for i, w := range wantWidgets {
		sortmap[w] = i
	}
	sort.Slice(widgets, func(i, j int) bool { return sortmap[widgets[i].Name] < sortmap[widgets[j].Name] })

	return zhttp.Template(w, "dashboard.gohtml", struct {
		Globals
		CountDomain    string
		SubSites       []string
		ShowRefs       string
		SelectedPeriod string
		PeriodStart    time.Time
		PeriodEnd      time.Time
		Filter         string
		Daily          bool
		ForcedDaily    bool
		Widgets        []Widget
	}{newGlobals(w, r),
		cd, subs, showRefs, r.URL.Query().Get("hl-period"), start, end, filter,
		daily, forcedDaily, widgets,
	})
}

func (h backend) pages(w http.ResponseWriter, r *http.Request) error {
	site := goatcounter.MustGetSite(r.Context())

	exclude := r.URL.Query().Get("exclude")
	filter := r.URL.Query().Get("filter")
	start, end, err := getPeriod(w, r, site)
	if err != nil {
		return err
	}
	daily, forcedDaily := getDaily(r, start, end)
	m, err := strconv.ParseInt(r.URL.Query().Get("max"), 10, 64)
	if err != nil {
		return err
	}
	max := int(m)

	// Load new totals unless this is for pagination.
	var (
		wg sync.WaitGroup

		totalTpl   []byte
		totalPages goatcounter.HitStat
		totalErr   error

		maxTotals int
		maxErr    error

		totalHits, totalUnique int
		totalCountErr          error
	)
	// Filtering instead of paginating: get new "totals" stats as well.
	// TODO: also re-render the the horizontal bar charts below, but this isn't
	// currently possible since not all data is linked to a path.
	if exclude == "" {
		wg.Add(1)
		go func() {
			defer zlog.Recover(func(l zlog.Log) zlog.Log { return l.FieldsRequest(r) })
			defer wg.Done()

			maxTotals, totalErr = totalPages.Totals(r.Context(), start, end, filter, daily)
			if totalErr != nil {
				return
			}

			totalTpl, totalErr = zhttp.ExecuteTpl("_dashboard_totals_row.gohtml", struct {
				Context context.Context
				Site    *goatcounter.Site
				Page    goatcounter.HitStat
				Daily   bool
				Max     int
			}{r.Context(), site, totalPages, daily, maxTotals})
		}()

		wg.Add(1)
		go func() {
			defer zlog.Recover(func(l zlog.Log) zlog.Log { return l.FieldsRequest(r) })
			defer wg.Done()

			max, maxErr = goatcounter.GetMax(r.Context(), start, end, filter, daily)
		}()

		wg.Add(1)
		go func() {
			defer zlog.Recover(func(l zlog.Log) zlog.Log { return l.FieldsRequest(r) })
			defer wg.Done()

			totalHits, totalUnique, totalCountErr = goatcounter.GetTotalCount(r.Context(), start, end, filter)
		}()
	}

	var pages goatcounter.HitStats
	totalDisplay, totalUniqueDisplay, more, err := pages.List(
		r.Context(), start, end, filter, strings.Split(exclude, ","), daily)
	if err != nil {
		return err
	}

	tpl, err := zhttp.ExecuteTpl("_dashboard_pages_rows.gohtml", struct {
		Context     context.Context
		Pages       goatcounter.HitStats
		Site        *goatcounter.Site
		PeriodStart time.Time
		PeriodEnd   time.Time
		Daily       bool
		ForcedDaily bool
		Max         int

		// Dummy values so template won't error out.
		Refs     bool
		ShowRefs string
	}{r.Context(), pages, site, start, end,
		daily, forcedDaily, int(max), false, ""})
	if err != nil {
		return err
	}

	paths := make([]string, len(pages))
	for i := range pages {
		paths[i] = pages[i].Path
	}

	wg.Wait()
	if totalErr != nil {
		return totalErr
	}
	if maxErr != nil {
		return maxErr
	}
	if totalCountErr != nil {
		return totalCountErr
	}

	return zhttp.JSON(w, map[string]interface{}{
		"rows":                 string(tpl),
		"totals":               string(totalTpl),
		"paths":                paths,
		"total_hits":           totalHits,
		"total_display":        totalDisplay,
		"total_unique":         totalUnique,
		"total_unique_display": totalUniqueDisplay,
		"max":                  max,
		"more":                 more,
	})
}

func (h backend) hchartDetail(w http.ResponseWriter, r *http.Request) error {
	start, end, err := getPeriod(w, r, goatcounter.MustGetSite(r.Context()))
	if err != nil {
		return err
	}

	v := zvalidate.New()
	name := r.URL.Query().Get("name")
	kind := r.URL.Query().Get("kind")
	v.Required("name", name)
	v.Include("kind", kind, []string{"browser", "system", "size", "topref"})
	v.Required("kind", kind)
	total := int(v.Integer("total", r.URL.Query().Get("total")))
	if v.HasErrors() {
		return v
	}

	var detail goatcounter.Stats
	switch kind {
	case "browser":
		err = detail.ListBrowser(r.Context(), name, start, end)
	case "system":
		err = detail.ListSystem(r.Context(), name, start, end)
	case "size":
		err = detail.ListSize(r.Context(), name, start, end)
	case "topref":
		err = detail.ByRef(r.Context(), start, end, name)
	}
	if err != nil {
		return err
	}

	return zhttp.JSON(w, map[string]interface{}{
		"html": string(goatcounter.HorizontalChart(r.Context(), detail, total, 10, false, true)),
	})
}

func (h backend) hchartMore(w http.ResponseWriter, r *http.Request) error {
	start, end, err := getPeriod(w, r, goatcounter.MustGetSite(r.Context()))
	if err != nil {
		return err
	}

	v := zvalidate.New()
	kind := r.URL.Query().Get("kind")
	v.Include("kind", kind, []string{"browser", "system", "location", "ref", "topref"})
	v.Required("kind", kind)
	total := int(v.Integer("total", r.URL.Query().Get("total")))
	offset := int(v.Integer("offset", r.URL.Query().Get("offset")))

	showRefs := ""
	if kind == "ref" {
		showRefs = r.URL.Query().Get("showrefs")
		v.Required("showrefs", "showRefs")
	}
	if v.HasErrors() {
		return v
	}

	var page goatcounter.Stats
	switch kind {
	case "browser":
		err = page.ListBrowsers(r.Context(), start, end, 6, offset)
	case "system":
		err = page.ListSystems(r.Context(), start, end, 6, offset)
	case "location":
		err = page.ListLocations(r.Context(), start, end, 6, offset)
	case "ref":
		err = page.ListRefsByPath(r.Context(), showRefs, start, end, offset)
	case "topref":
		err = page.ListTopRefs(r.Context(), start, end, offset)
	}
	if err != nil {
		return err
	}

	return zhttp.JSON(w, map[string]interface{}{
		"html": string(goatcounter.HorizontalChart(r.Context(), page, total, 6, kind != "location", false)),
		"more": page.More,
	})
}

func (h backend) updates(w http.ResponseWriter, r *http.Request) error {
	u := goatcounter.GetUser(r.Context())

	var up goatcounter.Updates
	err := up.List(r.Context(), u.SeenUpdatesAt)
	if err != nil {
		return err
	}

	seenat := u.SeenUpdatesAt
	err = u.SeenUpdates(r.Context())
	if err != nil {
		zlog.Field("user", fmt.Sprintf("%d", u.ID)).Error(err)
	}

	return zhttp.Template(w, "backend_updates.gohtml", struct {
		Globals
		Updates goatcounter.Updates
		SeenAt  time.Time
	}{newGlobals(w, r), up, seenat})
}

func (h backend) settings(w http.ResponseWriter, r *http.Request) error {
	return h.settingsTpl(w, r, nil)
}

func (h backend) settingsTpl(w http.ResponseWriter, r *http.Request, verr *zvalidate.Validator) error {
	var sites goatcounter.Sites
	err := sites.ListSubs(r.Context())
	if err != nil {
		return err
	}

	var exports goatcounter.Exports
	err = exports.List(r.Context())
	if err != nil {
		return err
	}

	var tokens goatcounter.APITokens
	err = tokens.List(r.Context())
	if err != nil {
		return err
	}

	del := map[string]interface{}{
		"ContactMe": r.URL.Query().Get("contact_me") == "true",
		"Reason":    r.URL.Query().Get("reason"),
	}

	return zhttp.Template(w, "backend_settings.gohtml", struct {
		Globals
		SubSites  goatcounter.Sites
		Validate  *zvalidate.Validator
		Timezones []*tz.Zone
		Delete    map[string]interface{}
		Exports   goatcounter.Exports
		APITokens goatcounter.APITokens
	}{newGlobals(w, r), sites, verr, tz.Zones, del, exports, tokens})
}

func (h backend) code(w http.ResponseWriter, r *http.Request) error {
	var sites goatcounter.Sites
	err := sites.ListSubs(r.Context())
	if err != nil {
		return err
	}

	cd := cfg.DomainCount
	if cd == "" {
		cd = goatcounter.MustGetSite(r.Context()).Domain()
		if cfg.Port != "" {
			cd += ":" + cfg.Port
		}
	}

	return zhttp.Template(w, "backend_code.gohtml", struct {
		Globals
		SubSites    goatcounter.Sites
		CountDomain string
	}{newGlobals(w, r), sites, cd})
}

func (h backend) ip(w http.ResponseWriter, r *http.Request) error {
	return zhttp.String(w, r.RemoteAddr)
}

func (h backend) saveSettings(w http.ResponseWriter, r *http.Request) error {
	v := zvalidate.New()

	args := struct {
		Cname      string                   `json:"cname"`
		LinkDomain string                   `json:"link_domain"`
		Settings   goatcounter.SiteSettings `json:"settings"`
		User       goatcounter.User         `json:"user"`
	}{}
	_, err := zhttp.Decode(r, &args)
	if err != nil {
		ferr, ok := err.(*formam.Error)
		if !ok || ferr.Code() != formam.ErrCodeConversion {
			return err
		}
		v.Append(ferr.Path(), "must be a number")

		// TODO: we return here because formam stops decoding on the first
		// error. We should really fix this in formam, but it's an incompatible
		// change.
		return h.settingsTpl(w, r, &v)
	}

	txctx, tx, err := zdb.Begin(r.Context())
	if err != nil {
		return err
	}
	defer tx.Rollback()

	user := goatcounter.GetUser(txctx)

	emailChanged := false
	if cfg.GoatcounterCom && args.User.Email != user.Email {
		emailChanged = true
	}

	user.Email = args.User.Email
	err = user.Update(txctx, emailChanged)
	if err != nil {
		var vErr *zvalidate.Validator
		if !errors.As(err, &vErr) {
			return err
		}
		v.Sub("user", "", err)
	}

	site := goatcounter.MustGetSite(txctx)
	site.Settings = args.Settings
	site.LinkDomain = args.LinkDomain
	if args.Cname != "" && !site.PlanCustomDomain(txctx) {
		return guru.New(http.StatusForbidden, "need a business plan to set custom domain")
	}

	makecert := false
	if args.Cname == "" {
		site.Cname = nil
	} else {
		if site.Cname == nil || *site.Cname != args.Cname {
			makecert = true // Make after we persisted to DB.
		}
		site.Cname = &args.Cname
	}

	err = site.Update(txctx)
	if err != nil {
		var vErr *zvalidate.Validator
		if !errors.As(err, &vErr) {
			return err
		}
		v.Sub("site", "", err)
	}

	if v.HasErrors() {
		return h.settingsTpl(w, r, &v)
	}

	err = tx.Commit()
	if err != nil {
		return err
	}

	if emailChanged {
		sendEmailVerify(site, user)
	}

	if makecert {
		ctx := goatcounter.NewContext(r.Context())
		bgrun.Run(func() {
			err := acme.Make(args.Cname)
			if err != nil {
				zlog.Field("domain", args.Cname).Error(err)
				return
			}

			err = site.UpdateCnameSetupAt(ctx)
			if err != nil {
				zlog.Field("domain", args.Cname).Error(err)
			}
		})
	}

	zhttp.Flash(w, "Saved!")
	return zhttp.SeeOther(w, "/settings")
}

func (h backend) importFile(w http.ResponseWriter, r *http.Request) error {
	v := zvalidate.New()
	replace := v.Boolean("replace", r.Form.Get("replace"))
	if v.HasErrors() {
		return v
	}

	file, head, err := r.FormFile("csv")
	if err != nil {
		return err
	}
	defer file.Close()

	var fp io.ReadCloser = file
	if strings.HasSuffix(head.Filename, ".gz") {
		fp, err = gzip.NewReader(file)
		if err != nil {
			return guru.Errorf(400, "could not read as gzip: %w", err)
		}
	}
	defer fp.Close()

	ctx := goatcounter.NewContext(r.Context())
	bgrun.Run(func() { goatcounter.Import(ctx, fp, replace, true) })

	zhttp.Flash(w, "Import started in the background; you’ll get an email when it’s done.")
	return zhttp.SeeOther(w, "/settings#tab-export")
}

func (h backend) startExport(w http.ResponseWriter, r *http.Request) error {
	r.ParseForm()

	v := zvalidate.New()
	startFrom := v.Integer("startFrom", r.Form.Get("startFrom"))
	if v.HasErrors() {
		return v
	}

	var export goatcounter.Export
	fp, err := export.Create(r.Context(), startFrom)
	if err != nil {
		return err
	}

	ctx := goatcounter.NewContext(r.Context())
	bgrun.Run(func() { export.Run(ctx, fp, true) })

	zhttp.Flash(w, "Export started in the background; you’ll get an email with a download link when it’s done.")
	return zhttp.SeeOther(w, "/settings#tab-export")
}

func (h backend) downloadExport(w http.ResponseWriter, r *http.Request) error {
	v := zvalidate.New()
	id := v.Integer("id", chi.URLParam(r, "id"))
	if v.HasErrors() {
		return v
	}

	var export goatcounter.Export
	err := export.ByID(r.Context(), id)
	if err != nil {
		return err
	}

	fp, err := os.Open(export.Path)
	if err != nil {
		if os.IsNotExist(err) {
			zhttp.FlashError(w, "It looks like there is no export yet.")
			return zhttp.SeeOther(w, "/settings#tab-export")
		}

		return err
	}
	defer fp.Close()

	err = header.SetContentDisposition(w.Header(), header.DispositionArgs{
		Type:     header.TypeAttachment,
		Filename: filepath.Base(export.Path),
	})
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/gzip")
	return zhttp.Stream(w, fp)
}

func (h backend) removeSubsiteConfirm(w http.ResponseWriter, r *http.Request) error {
	if !cfg.GoatcounterCom {
		return guru.New(400, "can only do this in SaaS mode")
	}

	v := zvalidate.New()
	id := v.Integer("id", chi.URLParam(r, "id"))
	if v.HasErrors() {
		return v
	}

	var s goatcounter.Site
	err := s.ByID(r.Context(), id)
	if err != nil {
		return err
	}

	return zhttp.Template(w, "backend_remove.gohtml", struct {
		Globals
		Site goatcounter.Site
	}{newGlobals(w, r), s})
}

func (h backend) removeSubsite(w http.ResponseWriter, r *http.Request) error {
	if !cfg.GoatcounterCom {
		return guru.New(400, "can only do this in SaaS mode")
	}

	v := zvalidate.New()
	id := v.Integer("id", chi.URLParam(r, "id"))
	if v.HasErrors() {
		return v
	}

	var s goatcounter.Site
	err := s.ByID(r.Context(), id)
	if err != nil {
		return err
	}

	err = s.Delete(r.Context())
	if err != nil {
		return err
	}

	zhttp.Flash(w, "Site ‘%s ’removed.", s.Code)
	return zhttp.SeeOther(w, "/settings#tab-additional-sites")
}

func (h backend) addSubsite(w http.ResponseWriter, r *http.Request) error {
	if !cfg.GoatcounterCom {
		return guru.New(400, "can only do this in SaaS mode")
	}

	args := struct {
		Code string `json:"code"`
	}{}
	_, err := zhttp.Decode(r, &args)
	if err != nil {
		return err
	}

	parent := goatcounter.MustGetSite(r.Context())
	site := goatcounter.Site{
		Code:     args.Code,
		Parent:   &parent.ID,
		Plan:     goatcounter.PlanChild,
		Settings: parent.Settings,
	}
	err = site.Insert(r.Context())
	if err != nil {
		zhttp.FlashError(w, err.Error())
		return zhttp.SeeOther(w, "/settings#tab-additional-sites")
	}

	zhttp.Flash(w, "Site ‘%s’ added.", site.Code)
	return zhttp.SeeOther(w, "/settings#tab-additional-sites")
}

func (h backend) purgeConfirm(w http.ResponseWriter, r *http.Request) error {
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	title := r.URL.Query().Get("match-title") == "on"

	var list goatcounter.HitStats
	err := list.ListPathsLike(r.Context(), path, title)
	if err != nil {
		return err
	}

	return zhttp.Template(w, "backend_purge.gohtml", struct {
		Globals
		PurgePath string
		List      goatcounter.HitStats
	}{newGlobals(w, r), path, list})
}

func (h backend) purge(w http.ResponseWriter, r *http.Request) error {
	ctx := goatcounter.NewContext(r.Context())
	bgrun.Run(func() {
		var list goatcounter.Hits
		err := list.Purge(ctx, r.Form.Get("path"), r.Form.Get("match-title") == "on")
		if err != nil {
			zlog.Error(err)
		}
	})

	zhttp.Flash(w, "Started in the background; may take about 10-20 seconds to fully process.")
	return zhttp.SeeOther(w, "/settings#tab-purge")
}

func hasPlan(site *goatcounter.Site) (bool, error) {
	if !cfg.GoatcounterCom || site.Plan == goatcounter.PlanChild ||
		site.Stripe == nil || site.FreePlan() || site.PayExternal() != "" {
		return false, nil
	}

	var customer struct {
		Subscriptions struct {
			Data []struct {
				CancelAtPeriodEnd bool            `json:"cancel_at_period_end"`
				CurrentPeriodEnd  zjson.Timestamp `json:"current_period_end"`
				Plan              struct {
					Quantity int `json:"quantity"`
				} `json:"plan"`
			} `json:"data"`
		} `json:"subscriptions"`
	}
	_, err := zstripe.Request(&customer, "GET",
		fmt.Sprintf("/v1/customers/%s", *site.Stripe), "")
	if err != nil {
		return false, err
	}

	if len(customer.Subscriptions.Data) == 0 {
		return false, nil
	}

	if customer.Subscriptions.Data[0].CancelAtPeriodEnd {
		return false, nil
	}

	return true, nil
}

func (h backend) delete(w http.ResponseWriter, r *http.Request) error {
	site := goatcounter.MustGetSite(r.Context())

	if cfg.GoatcounterCom {
		var args struct {
			Reason    string `json:"reason"`
			ContactMe bool   `json:"contact_me"`
		}
		_, err := zhttp.Decode(r, &args)
		if err != nil {
			zlog.Error(err)
		}

		has, err := hasPlan(site)
		if err != nil {
			return err
		}
		if has {
			zhttp.FlashError(w, "This site still has a Stripe subscription; cancel that first on the billing page.")
			q := url.Values{}
			q.Set("reason", args.Reason)
			q.Set("contact_me", fmt.Sprintf("%t", args.ContactMe))
			return zhttp.SeeOther(w, "/settings?"+q.Encode()+"#tab-delete")
		}

		if args.Reason != "" {
			bgrun.Run(func() {
				contact := "false"
				if args.ContactMe {
					var u goatcounter.User
					err := u.BySite(r.Context(), site.ID)
					if err != nil {
						zlog.Error(err)
					} else {
						contact = u.Email
					}
				}

				blackmail.Send("GoatCounter deletion",
					blackmail.From("GoatCounter deletion", cfg.EmailFrom),
					blackmail.To(cfg.EmailFrom),
					blackmail.Bodyf(`Deleted: %s (%d): contact_me: %s; reason: %s`,
						site.Code, site.ID, contact, args.Reason))
			})
		}
	}

	err := site.Delete(r.Context())
	if err != nil {
		return err
	}

	if site.Parent != nil {
		var p goatcounter.Site
		err := p.ByID(r.Context(), *site.Parent)
		if err != nil {
			return err
		}
		return zhttp.SeeOther(w, p.URL())
	}

	return zhttp.SeeOther(w, "https://"+cfg.Domain)
}

func getPeriod(w http.ResponseWriter, r *http.Request, site *goatcounter.Site) (time.Time, time.Time, error) {
	var start, end time.Time

	if d := r.URL.Query().Get("period-start"); d != "" {
		var err error
		start, err = time.ParseInLocation("2006-01-02", d, site.Settings.Timezone.Loc())
		if err != nil {
			return start, end, guru.Errorf(400, "Invalid start date: %q", d)
		}
	}
	if d := r.URL.Query().Get("period-end"); d != "" {
		var err error
		end, err = time.ParseInLocation("2006-01-02 15:04:05", d+" 23:59:59", site.Settings.Timezone.Loc())
		if err != nil {
			return start, end, guru.Errorf(400, "Invalid end date: %q", d)
		}
	}

	// Allow viewing a week before the site was created at the most.
	c := site.CreatedAt.Add(-24 * time.Hour * 7)
	if start.Before(c) {
		y, m, d := c.In(site.Settings.Timezone.Loc()).Date()
		start = time.Date(y, m, d, 0, 0, 0, 0, site.Settings.Timezone.Loc())
	}

	return start.UTC(), end.UTC(), nil
}

func getDaily(r *http.Request, start, end time.Time) (daily bool, forced bool) {
	if end.Sub(start).Hours()/24 >= DailyView {
		return true, true
	}
	d := strings.ToLower(r.URL.Query().Get("daily"))
	return d == "on" || d == "true", false
}
