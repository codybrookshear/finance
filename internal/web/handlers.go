package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// base is what every page's layout needs.
type base struct {
	Page     string
	Email    string
	LastSync *time.Time
	NextSync string // "next ~1:40 AM", or "overdue" when syncs seem to have stopped
}

// The sync schedule, as in deploy/finance/finance-sync.timer: every 3 hours
// at :40, starting 1:40 AM local time.
const (
	syncFirstHour = 1
	syncEvery     = 3 * time.Hour
	syncMinute    = 40
)

func (s *Server) base(r *http.Request, page string) (base, error) {
	last, err := s.cfg.DB.LastSync(r.Context())
	b := base{Page: page, Email: userEmail(r.Context()), LastSync: last}
	now := time.Now().In(s.cfg.Location)
	if last != nil && now.Sub(*last) > syncEvery+time.Hour {
		b.NextSync = "overdue"
	} else {
		b.NextSync = "next ~" + nextSync(now).Format("3:04 PM")
	}
	return b, err
}

// nextSync is the first scheduled sync after now (in now's location).
func nextSync(now time.Time) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), syncFirstHour, syncMinute, 0, 0, now.Location())
	for !t.After(now) {
		t = t.Add(syncEvery)
	}
	return t
}

// txnRow is one transaction in the list. Day is set on the first row of each
// day, for the date separator.
type txnRow struct {
	Txn
	Day string
}

// rowsPage is what the "rows" template renders.
type rowsPage struct {
	Rows       []txnRow
	Categories []Category // for each row's category picker
	Next       string     // URL of the next page; "" at the end
}

type txnPage struct {
	base
	rowsPage
	Q, Account, From, To string // the filter as typed, echoed into the form
	Category             string
	Accounts             []Account
	Count                int
	Totals               []Total
	Filtered             bool
}

func (s *Server) transactions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	p := txnPage{Q: clip(q.Get("q"), 200), Account: clip(q.Get("account"), 200)}
	if c := q.Get("category"); c == "none" || c == "learned" {
		p.Category = c
	} else if id, ok := parseID(c); ok && id != nil {
		p.Category = c
	}
	f := TxnFilter{Query: p.Q, AccountID: p.Account, Category: p.Category, Limit: 50}
	from, to := q.Get("from"), q.Get("to")
	fromDay, fromOK := s.parseDay(from)
	toDay, toOK := s.parseDay(to)
	if fromOK && toOK && toDay.Before(fromDay) { // a reversed range: take it the right way round
		from, to, fromDay, toDay = to, from, toDay, fromDay
	}
	if fromOK {
		p.From, f.From = from, &fromDay
	}
	if toOK {
		end := toDay.AddDate(0, 0, 1) // "to" is inclusive
		p.To, f.To = to, &end
	}
	if a := q.Get("after"); a != "" {
		c, err := parseCursor(a)
		if err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		f.After = c
	}
	p.Filtered = p.Q != "" || p.Account != "" || p.From != "" || p.To != "" || p.Category != ""

	txns, next, err := s.cfg.DB.Transactions(ctx, f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	prevDay := "" // the day the previous page ended on: don't repeat its header
	if _, ok := s.parseDay(q.Get("day")); ok && f.After != nil {
		prevDay = q.Get("day")
	}
	var lastDay string
	p.Rows, lastDay = s.groupByDay(txns, prevDay)
	if next != nil {
		q.Set("after", next.String())
		q.Set("day", lastDay)
		p.Next = "/transactions?" + q.Encode()
	}
	if p.Categories, err = s.cfg.DB.Categories(ctx); err != nil {
		s.fail(w, r, err)
		return
	}

	// htmx requests name the element they'll replace.
	htmx := r.Header.Get("HX-Request") == "true"
	// Infinite scroll targets #more; a Spending category opening targets its own list.
	if target := r.Header.Get("HX-Target"); htmx && (target == "more" || strings.HasPrefix(target, "cat-")) {
		s.render(w, r, "transactions", "rows", p.rowsPage)
		return
	}
	if p.Count, p.Totals, err = s.cfg.DB.TxnSummary(ctx, f); err != nil {
		s.fail(w, r, err)
		return
	}
	if htmx && r.Header.Get("HX-Target") == "results" { // the search changed
		s.render(w, r, "transactions", "results", p)
		return
	}
	if p.base, err = s.base(r, "transactions"); err != nil {
		s.fail(w, r, err)
		return
	}
	if p.Accounts, err = s.cfg.DB.Accounts(ctx); err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "transactions", "layout.html", p)
}

// groupByDay marks the first row of each local day with a label for the
// date separator, and returns the last row's day (YYYY-MM-DD).
func (s *Server) groupByDay(txns []Txn, prevDay string) ([]txnRow, string) {
	now := time.Now().In(s.cfg.Location)
	today, yesterday := now.Format(time.DateOnly), now.AddDate(0, 0, -1).Format(time.DateOnly)
	rows := make([]txnRow, len(txns))
	for i, t := range txns {
		rows[i].Txn = t
		at := t.At.In(s.cfg.Location)
		day := at.Format(time.DateOnly)
		if day == prevDay {
			continue
		}
		prevDay = day
		switch {
		case day == today:
			rows[i].Day = "Today"
		case day == yesterday:
			rows[i].Day = "Yesterday"
		case at.Year() == now.Year():
			rows[i].Day = at.Format("Mon, Jan 2")
		default:
			rows[i].Day = at.Format("Mon, Jan 2, 2006")
		}
	}
	return rows, prevDay
}

// spendLine is a category's spending in the shown month, with last month's
// for comparison.
type spendLine struct {
	Line
	Last    string // last month's spending in this category ("" if none)
	HasLast bool
	Over    bool // spent more than last month
}

// shareSeg is a category's slice of the month's spending bar, in thousandths
// of the bar's width.
type shareSeg struct {
	Line
	X, W, Mid int
	Percent   int
	Icon      bool // wide enough to show the category's icon
}

// forecast is the current month's income outlook: income is expected to
// match what you set, else last month's, so spending early in the month
// isn't "underwater".
type forecast struct {
	Expected string // expected income for the whole month (never less than what's arrived)
	Left     string // Expected minus spending so far
	LeftNeg  bool
	Source   string // "set" (by you), "last" (month's income) or "arrived" (more than expected has come in)
	Setting  string // the value you set ("" if none), for the form
}

type spendingPage struct {
	base
	YearMonth, Prev, Next string
	From, To              string // the month's first and last day
	Month                 Month
	Cats                  []spendLine
	Shares                []shareSeg
	Income                []Line
	Forecast              *forecast
	HasData               bool
}

var monthRE = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])$`)

func (s *Server) spending(w http.ResponseWriter, r *http.Request) {
	now := time.Now().In(s.cfg.Location)
	thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, s.cfg.Location)
	since := thisMonth.AddDate(-3, 0, 0)
	months, err := s.cfg.DB.Spending(r.Context(), s.cfg.Location.String(), since)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	byMonth := map[string]Month{}
	oldest := thisMonth.Format("2006-01")
	for _, m := range months {
		byMonth[m.YearMonth] = m
		oldest = min(oldest, m.YearMonth) // YYYY-MM sorts as dates do
	}

	// The month shown: ?month=YYYY-MM, else this one; never a future month.
	cur := thisMonth.Format("2006-01")
	ym := cur
	if t, err := time.Parse("2006-01", r.URL.Query().Get("month")); err == nil {
		ym = min(max(t.Format("2006-01"), oldest), cur)
	}
	first, _ := time.ParseInLocation("2006-01", ym, s.cfg.Location)
	p := spendingPage{
		YearMonth: ym,
		From:      first.Format(time.DateOnly),
		To:        first.AddDate(0, 1, -1).Format(time.DateOnly),
	}
	if ym > oldest {
		p.Prev = first.AddDate(0, -1, 0).Format("2006-01")
	}
	if ym < cur {
		p.Next = first.AddDate(0, 1, 0).Format("2006-01")
	}
	m, ok := byMonth[ym]
	if !ok {
		m = Month{YearMonth: ym, Spent: "0.00", Income: "0.00", Net: "0.00"}
	}
	p.Month, p.HasData = m, ok

	// Each category row compares with the same category last month.
	last := map[string]string{} // by label
	for _, l := range byMonth[first.AddDate(0, -1, 0).Format("2006-01")].Categories {
		last[l.Label] = l.Amount
	}
	for _, l := range m.Categories {
		l.URL = s.categoryURL(p.From, p.To, l.CategoryID)
		sl := spendLine{Line: l}
		if lc, ok := toCents(last[l.Label]); ok && lc > 0 {
			sl.HasLast, sl.Last = true, last[l.Label]
			spent, _ := toCents(l.Amount)
			sl.Over = spent > lc
		}
		p.Cats = append(p.Cats, sl)
	}
	p.Shares = shares(p.Cats)
	for _, l := range m.IncomeCategories {
		l.URL = s.categoryURL(p.From, p.To, l.CategoryID)
		p.Income = append(p.Income, l)
	}
	if ym == cur {
		set, err := s.cfg.DB.ExpectedIncome(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		p.Forecast = forecastFor(byMonth[first.AddDate(0, -1, 0).Format("2006-01")], set, m)
	}
	if p.base, err = s.base(r, "spending"); err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "spending", "layout.html", p)
}

// categoryURL is the month's transactions in one category (nil = none).
func (s *Server) categoryURL(from, to string, id *int32) string {
	q := url.Values{"from": {from}, "to": {to}, "category": {"none"}}
	if id != nil {
		q.Set("category", strconv.Itoa(int(*id)))
	}
	return "/transactions?" + q.Encode()
}

// shares lays the month's spending out as a bar: each category with money
// spent takes a width in proportion to its amount (the widest first, as the
// lines are).
func shares(cats []spendLine) []shareSeg {
	var total int64
	for _, c := range cats {
		if n, _ := toCents(c.Amount); n > 0 {
			total += n
		}
	}
	var segs []shareSeg
	var before int64
	for _, c := range cats {
		n, _ := toCents(c.Amount)
		if n <= 0 {
			continue
		}
		x, end := int(before*1000/total), int((before+n)*1000/total)
		segs = append(segs, shareSeg{Line: c.Line, X: x, W: end - x, Mid: (x + end) / 2,
			Percent: int((n*100 + total/2) / total), Icon: end-x >= 70})
		before += n
	}
	return segs
}

// forecastFor expects this month's income to be what you set, else last
// month's income, or what has already arrived if that's more. It returns nil
// when there's nothing to go on.
func forecastFor(lastMonth Month, set *string, m Month) *forecast {
	f := forecast{Source: "last"}
	var expected int64
	if set != nil {
		expected, _ = toCents(*set)
		f.Source, f.Setting = "set", *set
	} else if expected, _ = toCents(lastMonth.Income); expected <= 0 {
		return nil
	}
	if got, _ := toCents(m.Income); got > expected {
		expected, f.Source = got, "arrived"
	}
	spent, _ := toCents(m.Spent)
	f.Expected, f.Left, f.LeftNeg = fromCents(expected), fromCents(expected-spent), expected < spent
	return &f
}

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := struct {
		base
		Group, GroupLabel      string // what the headline and chart show
		Groups                 []struct{ Key, Label string }
		Totals                 []Total
		Accounts               []Account
		ChartDays, ChartValues string // comma-separated, for app.js
	}{Group: "total", Groups: ChartGroups}
	for _, g := range ChartGroups {
		if g.Key == r.URL.Query().Get("show") {
			p.Group = g.Key
		}
		if g.Key == p.Group {
			p.GroupLabel = g.Label
		}
	}
	var err error
	if p.Totals, err = s.cfg.DB.NetWorthNow(ctx, p.Group); err != nil {
		s.fail(w, r, err)
		return
	}
	if p.Accounts, err = s.cfg.DB.Accounts(ctx); err != nil {
		s.fail(w, r, err)
		return
	}
	points, err := s.cfg.DB.NetWorth(ctx, p.Group)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(points) >= 2 {
		days := make([]string, len(points))
		vals := make([]string, len(points))
		for i, pt := range points {
			days[i], vals[i] = pt.Day.Format(time.DateOnly), pt.Value
		}
		p.ChartDays, p.ChartValues = strings.Join(days, ","), strings.Join(vals, ",")
	}
	if p.base, err = s.base(r, "accounts"); err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "accounts", "layout.html", p)
}

// parseDay reads a YYYY-MM-DD date as local midnight.
func (s *Server) parseDay(v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(time.DateOnly, v, s.cfg.Location)
	return t, err == nil
}

// clip trims s and cuts it to at most n bytes without splitting a character.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// commas renders 1358 as "1,358".
func commas(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return "-" + commas(-n)
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}
