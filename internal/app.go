package internal

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	ics "github.com/arran4/golang-ical"
	"github.com/getsentry/sentry-go"
	sentrygin "github.com/getsentry/sentry-go/gin"
	"github.com/gin-gonic/gin"
)

//go:embed courses.json
var coursesJson string

//go:embed buildings.json
var buildingsJson string

//go:embed static
var static embed.FS

//go:embed filters/*.json
var filtersFS embed.FS

// Version is injected at build time by the compiler with the correct git-commit-sha or "dev" in development
var Version = "dev"

type App struct {
	engine *gin.Engine

	courseReplacements   []*Replacement
	buildingReplacements map[string]string
	berlinLocation       *time.Location
}

type Replacement struct {
	key   string
	value string
}

type FilterRule struct {
	Summary  string `json:"summary"`
	Time     string `json:"time"`
	Action   string `json:"action"` // "keep" or "delete"
	Priority int    `json:"priority,omitempty"`
}

type Filter struct {
	Rules []FilterRule `json:"rules"`
}

type TimeRange struct {
	DayOfWeek time.Weekday
	Start     time.Duration // minutes since midnight
	End       time.Duration // minutes since midnight
}

func parseTimeRange(timeStr string) (*TimeRange, error) {
	parts := strings.Split(timeStr, " ")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid time format: %s", timeStr)
	}
	dayStr := strings.ToLower(parts[0])
	timeRangeStr := parts[1]

	var day time.Weekday
	switch dayStr {
	case "sun":
		day = time.Sunday
	case "mon":
		day = time.Monday
	case "tue":
		day = time.Tuesday
	case "wed":
		day = time.Wednesday
	case "thu":
		day = time.Thursday
	case "fri":
		day = time.Friday
	case "sat":
		day = time.Saturday
	default:
		return nil, fmt.Errorf("invalid day: %s", dayStr)
	}

	timeParts := strings.Split(timeRangeStr, "-")
	if len(timeParts) != 2 {
		return nil, fmt.Errorf("invalid time range: %s", timeRangeStr)
	}

	start, err := parseTime(timeParts[0])
	if err != nil {
		return nil, err
	}
	end, err := parseTime(timeParts[1])
	if err != nil {
		return nil, err
	}

	return &TimeRange{DayOfWeek: day, Start: start, End: end}, nil
}

func parseTime(timeStr string) (time.Duration, error) {
	t, err := time.Parse("15:04", timeStr)
	if err != nil {
		return 0, err
	}
	return time.Duration(t.Hour()*60+t.Minute()) * time.Minute, nil
}

func (tr *TimeRange) matches(eventTime time.Time) bool {
	if eventTime.Weekday() != tr.DayOfWeek {
		return false
	}
	minutes := time.Duration(eventTime.Hour()*60+eventTime.Minute()) * time.Minute
	return minutes >= tr.Start && minutes < tr.End
}

// for sorting replacements by length, then alphabetically
func (r1 *Replacement) isLessThan(r2 *Replacement) bool {
	if len(r1.key) != len(r2.key) {
		return len(r1.key) > len(r2.key)
	}
	if r1.key != r2.key {
		return r1.key < r2.key
	}
	return r1.value < r2.value
}

func newApp() (*App, error) {
	a := App{}

	// Load Berlin time zone for filter time rules
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		return nil, err
	}
	a.berlinLocation = loc

	// courseReplacements is a map of course names to shortened names.
	// We sort it by length, then alphabetically to ensure a consistent execution order
	var rawCourseReplacements map[string]string
	if err := json.Unmarshal([]byte(coursesJson), &rawCourseReplacements); err != nil {
		return nil, err
	}
	for key, value := range rawCourseReplacements {
		a.courseReplacements = append(a.courseReplacements, &Replacement{key, value})
	}
	sort.Slice(a.courseReplacements, func(i, j int) bool { return a.courseReplacements[i].isLessThan(a.courseReplacements[j]) })
	// buildingReplacements is a map of room numbers to building names
	if err := json.Unmarshal([]byte(buildingsJson), &a.buildingReplacements); err != nil {
		return nil, err
	}
	return &a, nil
}

func (a *App) loadFilters(jsonFilterTokens []string) (*Filter, error) {
	if len(jsonFilterTokens) == 0 {
		return nil, nil
	}

	combined := Filter{}
	var loadErrors []string
	for _, token := range jsonFilterTokens {
		if token == "" || token == "none" {
			continue
		}
		filePath := fmt.Sprintf("filters/%s.json", token)
		data, err := filtersFS.ReadFile(filePath)
		if err != nil {
			loadErrors = append(loadErrors, fmt.Sprintf("%s: %v", token, err))
			continue
		}
		var filter Filter
		if err := json.Unmarshal(data, &filter); err != nil {
			loadErrors = append(loadErrors, fmt.Sprintf("%s: %v", token, err))
			continue
		}
		// Set default priority to 1 if not set
		for i := range filter.Rules {
			if filter.Rules[i].Priority == 0 {
				filter.Rules[i].Priority = 1
			}
		}
		combined.Rules = append(combined.Rules, filter.Rules...)
	}

	if len(combined.Rules) == 0 && len(loadErrors) > 0 {
		return nil, fmt.Errorf(strings.Join(loadErrors, "; "))
	}
	if len(combined.Rules) == 0 {
		return nil, nil
	}

	// Sort rules by priority (ascending), stable sort maintains file and within-file order for equal priorities
	sort.SliceStable(combined.Rules, func(i, j int) bool {
		return combined.Rules[i].Priority < combined.Rules[j].Priority
	})

	return &combined, nil
}

func (a *App) Run() error {
	if err := sentry.Init(sentry.ClientOptions{
		Dsn:              "https://2fbc80ad1a99406cb72601d6a47240ce@glitch.exgen.io/4",
		Release:          Version,
		AttachStacktrace: true,
		EnableTracing:    true,
		// Specify a fixed sample rate: 10% will do for now
		TracesSampleRate: 0.1,
	}); err != nil {
		fmt.Printf("Sentry initialization failed: %v\n", err)
	}

	// Create app struct
	a, err := newApp()
	if err != nil {
		return err
	}

	// Setup Gin with sentry traces, logger and routes
	gin.SetMode("release")
	a.engine = gin.New()
	a.engine.Use(sentrygin.New(sentrygin.Options{}))
	logger := gin.LoggerWithConfig(gin.LoggerConfig{SkipPaths: []string{"/health"}})
	a.engine.Use(logger, gin.Recovery())
	a.configRoutes()

	// Start the engines
	return a.engine.Run(":4321")
}

func (a *App) configRoutes() {
	a.engine.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
		})
	})
	a.engine.Any("/", a.handleIcal)
	f := http.FS(static)
	a.engine.StaticFS("/files/", f)
	a.engine.NoMethod(func(c *gin.Context) {
		c.AbortWithStatus(http.StatusNotImplemented)
	})
}

func getUrl(c *gin.Context) (string, []string) {
	stud := c.Query("pStud")
	pers := c.Query("pPers")
	token := c.Query("pToken")

	var jsonFilterTokens []string
	for _, rawValue := range c.QueryArray("jsonFilter") {
		for _, token := range strings.Split(rawValue, ",") {
			token = strings.TrimSpace(token)
			if token != "" {
				jsonFilterTokens = append(jsonFilterTokens, token)
			}
		}
	}
	if len(jsonFilterTokens) == 0 {
		if raw := c.Query("jsonFilter"); raw != "" {
			for _, token := range strings.Split(raw, ",") {
				token = strings.TrimSpace(token)
				if token != "" {
					jsonFilterTokens = append(jsonFilterTokens, token)
				}
			}
		}
	}
	if (stud == "" && pers == "") || token == "" {
		// Missing parameters: just serve our landing page
		f, err := static.Open("static/index.html")
		if err != nil {
			sentry.CaptureException(err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, err)
			return "", nil
		}

		if _, err := io.Copy(c.Writer, f); err != nil {
			sentry.CaptureException(err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, err)
			return "", nil
		}
		return "", nil
	}
	if len(jsonFilterTokens) == 0 {
		jsonFilterTokens = nil
	}

	if stud == "" {
		return fmt.Sprintf("https://campus.tum.de/tumonlinej/ws/termin/ical?pPers=%s&pToken=%s", pers, token), jsonFilterTokens
	}
	return fmt.Sprintf("https://campus.tum.de/tumonlinej/ws/termin/ical?pStud=%s&pToken=%s", stud, token), jsonFilterTokens
}

func (a *App) handleIcal(c *gin.Context) {
	url, jsonFilterTokens := getUrl(c)
	if url == "" {
		return
	}
	resp, err := http.Get(url)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("HTTP error from calendar API: %d %s\n", resp.StatusCode, resp.Status)
		c.AbortWithStatusJSON(http.StatusInternalServerError, fmt.Errorf("calendar API returned %d", resp.StatusCode))
		return
	}

	all, err := io.ReadAll(resp.Body)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, err)
		return
	}
	cleaned, err := a.getCleanedCalendar(all, jsonFilterTokens)
	if err != nil {
		sentry.CaptureException(err)
		fmt.Printf("Error in getCleanedCalendar: %v\n", err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	response := []byte(cleaned.Serialize())
	c.Header("Content-Type", "text/calendar")
	c.Header("Content-Length", fmt.Sprintf("%d", len(response)))

	if _, err := c.Writer.Write(response); err != nil {
		sentry.CaptureException(err)
	}
}

func (a *App) getCleanedCalendar(all []byte, jsonFilterTokens []string) (*ics.Calendar, error) {
	cal, err := ics.ParseCalendar(strings.NewReader(string(all)))
	if err != nil {
		return nil, err
	}

	filter, err := a.loadFilters(jsonFilterTokens)
	if err != nil {
		fmt.Printf("Failed to load filters %v: %v\n", jsonFilterTokens, err)
		filter = nil
	}

	// Create map that tracks if we have allready seen a lecture name & datetime (e.g. "lecturexyz-1.2.2024 10:00" -> true)
	hasLecture := make(map[string]bool)
	var newComponents []ics.Component // saves the components we keep because they are not duplicated

	for _, component := range cal.Components {
		switch component.(type) {
		case *ics.VEvent:
			event := component.(*ics.VEvent)
			dedupKey := fmt.Sprintf("%s-%s", event.GetProperty(ics.ComponentPropertySummary).Value, event.GetProperty(ics.ComponentPropertyDtStart))
			if _, ok := hasLecture[dedupKey]; ok {
				continue
			}
			hasLecture[dedupKey] = true // mark event as seen
			keepEvent := a.cleanEvent(event, filter)
			if keepEvent {
				newComponents = append(newComponents, event)
			}
		default: // keep everything that is not an event (metadata etc.)
			newComponents = append(newComponents, component)
		}
	}
	cal.Components = newComponents
	return cal, nil
}

// matches tags like (IN0001) or [MA2012] and everything after.
// unfortunate also matches wrong brackets like [MA123) but hey…
var reTag = regexp.MustCompile(` ?[\[(](ED|MW|SOM|CIT|MA|IN|WI|WIB|CH)[0-9]+((_|-|,|n)[a-zA-Z0-9]+)*[a-z]?[\])].*`)

// Matches location and teacher from language course title
var reLoc = regexp.MustCompile(` ?(München|Garching|Weihenstephan).+`)

// Matches repeated whitespaces
var reSpace = regexp.MustCompile(`\s\s+`)

var unneeded = []string{
	"Standardgruppe",
	"PR",
	"VO",
	"FA",
	"VI",
	"TT",
	"UE",
	"SE",
	"(Limited places)",
	"(Online)",
}

// matches strings like: (5612.03.017), (5612.EG.017), (5612.EG.010B)
var reNavigaTUM = regexp.MustCompile(`\((\d{4})\.[a-zA-Z0-9]{2}\.\d{3}[A-Z]?\)`)

func (a *App) cleanEvent(event *ics.VEvent, filter *Filter) bool {
	summary := ""
	keepEvent := true
	if s := event.GetProperty(ics.ComponentPropertySummary); s != nil {
		summary = strings.ReplaceAll(s.Value, "\\", "")
	}

	// Apply JSON filter if present
	if filter != nil {
		for _, rule := range filter.Rules {
			if rule.Summary != "" && !strings.Contains(summary, rule.Summary) {
				continue // summary doesn't match
			}
			if rule.Time != "" {
				timeRange, err := parseTimeRange(rule.Time)
				if err != nil {
					fmt.Printf("Invalid time range in filter: %v\n", err)
					continue
				}
				dtStart := event.GetProperty(ics.ComponentPropertyDtStart)
				if dtStart == nil {
					continue
				}
				eventTime, err := time.Parse("20060102T150405Z", dtStart.Value)
				if err != nil {
					// Try without Z
					eventTime, err = time.Parse("20060102T150405", dtStart.Value)
					if err != nil {
						continue
					}
				}
				berlinTime := eventTime.In(a.berlinLocation)
				if !timeRange.matches(berlinTime) {
					continue // time doesn't match
				}
			}
			// Rule matches
			if rule.Action == "keep" {
				keepEvent = true
			} else if rule.Action == "delete" {
				keepEvent = false
			}
			break // First matching rule applies
		}
	}

	description := ""
	if d := event.GetProperty(ics.ComponentPropertyDescription); d != nil {
		description = strings.ReplaceAll(d.Value, "\\", "")
	}

	location := ""
	if l := event.GetProperty(ics.ComponentPropertyLocation); l != nil {
		location = strings.ReplaceAll(l.Value, "\\", "")
	}

	// legacy filter tokens are converted into filter.Rules in getCleanedCalendar

	//Remove the TAG and anything after e.g.: (IN0001) or [MA0001]
	summary = reTag.ReplaceAllString(summary, "")
	//remove location and teacher from language course title
	summary = reLoc.ReplaceAllString(summary, "")
	summary = reSpace.ReplaceAllString(summary, "")
	for _, replace := range unneeded {
		summary = strings.ReplaceAll(summary, replace, "")
	}

	summary = strings.TrimSuffix(summary, " , ") // remove trailing space comma space

	event.SetSummary(summary)

	//Remember the old title in the description
	description = summary + "\n" + description

	results := reNavigaTUM.FindStringSubmatch(location)
	if len(results) != 2 {
		results = reNavigaTUM.FindStringSubmatch(description) // attempt to find any location info in desc if none is found in location
	}
	if len(results) == 2 {
		if building, ok := a.buildingReplacements[results[1]]; ok {
			description = location + "\n" + description
			event.SetLocation(building)
		}
		roomID := reNavigaTUM.FindString(location)
		if roomID == "" {
			roomID = reNavigaTUM.FindString(description)
		}
		if roomID != "" {
			roomID = strings.Trim(roomID, "()")
			description = fmt.Sprintf("https://nav.tum.de/room/%s\n%s", roomID, description)
		}
	}
	event.SetDescription(description)

	// set title on summary:
	for _, repl := range a.courseReplacements {
		summary = strings.ReplaceAll(summary, repl.key, repl.value)
	}
	event.SetSummary(summary)
	switch event.GetProperty(ics.ComponentPropertyStatus).Value {
	case "CONFIRMED":
		event.SetStatus(ics.ObjectStatusConfirmed)
	case "CANCELLED":
		event.SetStatus(ics.ObjectStatusCancelled)
	case "TENTATIVE":
		event.SetStatus(ics.ObjectStatusTentative)
	}
	return keepEvent
}
