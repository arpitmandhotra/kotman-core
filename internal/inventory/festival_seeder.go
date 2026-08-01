package inventory

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// FestivalSeeder seeds the festival_calendars table with known Indian D2C
// commerce events. It is idempotent — running it multiple times is safe
// because it uses ON CONFLICT (festival_name, year) DO UPDATE.
//
// Fixed-date events derive their date purely from the calendar year.
// Variable-date (lunar/astrological) events are pre-computed per year and
// must be updated by an operator when dates shift beyond the seed window.
type FestivalSeeder struct {
	pg *gorm.DB
}

// NewFestivalSeeder constructs a FestivalSeeder.
func NewFestivalSeeder(pg *gorm.DB) *FestivalSeeder {
	return &FestivalSeeder{pg: pg}
}

// SeedUpcomingYears seeds festival data for the current year, next year, and
// the year after. Call this once at startup and optionally re-run annually.
func (fs *FestivalSeeder) SeedUpcomingYears(ctx context.Context) {
	currentYear := time.Now().Year()
	for _, yr := range []int{currentYear - 1, currentYear, currentYear + 1} {
		fs.seedYear(ctx, yr)
	}
}

func (fs *FestivalSeeder) seedYear(ctx context.Context, year int) {
	entries := buildFestivalCalendar(year)
	for _, entry := range entries {
		e := entry // avoid loop var capture

		if err := fs.pg.WithContext(ctx).
			Clauses(clause.OnConflict{
				Columns: []clause.Column{
					{Name: "festival_name"},
					{Name: "year"},
				},
				DoUpdates: clause.AssignmentColumns([]string{
					"event_type",
					"start_date",
					"end_date",
					"is_date_fixed",
					"expected_uplift_percent",
					"notes",
					"updated_at",
				}),
			}).
			Create(&e).Error; err != nil {
			slog.Error("festival seeder: failed to upsert festival",
				"festival", e.FestivalName, "year", e.Year, "error", err)
		}
	}
	slog.Info("festival seeder: seeded festival calendar", "year", year, "count", len(entries))
}

// ==========================================
// FESTIVAL DATE DEFINITIONS
// ==========================================

// buildFestivalCalendar returns all Indian D2C commerce events for a given year.
// Uplift percentages are indicative industry baselines for Indian D2C brands.
func buildFestivalCalendar(year int) []domain.FestivalCalendar {
	// Variable dates change year-to-year. Dates below are sourced from
	// the Hindu calendar; update when almanac data is available beyond 2027.
	variableDates := variableDateMap(year)

	var festivals []domain.FestivalCalendar

	// ---- FIXED-DATE EVENTS ----

	festivals = append(festivals, domain.FestivalCalendar{
		FestivalName:          "Republic Day Sale",
		Year:                  year,
		EventType:             "sale",
		StartDate:             date(year, time.January, 24), // 3-day run-up
		EndDate:               date(year, time.January, 26),
		IsDateFixed:           true,
		ExpectedUpliftPercent: 18.0,
		Notes:                 "Republic Day (26 Jan). Fixed date. Run-up typically starts 24 Jan.",
	})

	festivals = append(festivals, domain.FestivalCalendar{
		FestivalName:          "Independence Day Sale",
		Year:                  year,
		EventType:             "sale",
		StartDate:             date(year, time.August, 13), // run-up 13–15 Aug
		EndDate:               date(year, time.August, 15),
		IsDateFixed:           true,
		ExpectedUpliftPercent: 20.0,
		Notes:                 "Independence Day (15 Aug). Fixed date. Run-up starts 13 Aug.",
	})

	festivals = append(festivals, domain.FestivalCalendar{
		FestivalName:          "End of Season Sale (Summer)",
		Year:                  year,
		EventType:             "sale",
		StartDate:             date(year, time.July, 1),
		EndDate:               date(year, time.July, 15),
		IsDateFixed:           true,
		ExpectedUpliftPercent: 35.0,
		Notes:                 "Summer EOSS — typically first two weeks of July for fashion/apparel D2C brands.",
	})

	festivals = append(festivals, domain.FestivalCalendar{
		FestivalName:          "End of Season Sale (Winter)",
		Year:                  year,
		EventType:             "sale",
		StartDate:             date(year, time.January, 1),
		EndDate:               date(year, time.January, 15),
		IsDateFixed:           true,
		ExpectedUpliftPercent: 35.0,
		Notes:                 "Winter EOSS — typically first two weeks of January for fashion/apparel D2C brands.",
	})

	// ---- VARIABLE-DATE FESTIVALS ----

	if d, ok := variableDates["Holi"]; ok {
		festivals = append(festivals, domain.FestivalCalendar{
			FestivalName:          "Holi",
			Year:                  year,
			EventType:             "festival",
			StartDate:             d.AddDate(0, 0, -5), // pre-Holi shopping window
			EndDate:               d,
			IsDateFixed:           false,
			ExpectedUpliftPercent: 22.0,
			Notes:                 "Holi (variable date). 5-day pre-festival shopping window included.",
		})
	}

	if d, ok := variableDates["Dussehra"]; ok {
		festivals = append(festivals, domain.FestivalCalendar{
			FestivalName:          "Dussehra",
			Year:                  year,
			EventType:             "festival",
			StartDate:             d.AddDate(0, 0, -7), // Navratri shopping window
			EndDate:               d,
			IsDateFixed:           false,
			ExpectedUpliftPercent: 30.0,
			Notes:                 "Dussehra / Vijayadashami (variable date). Shopping window includes Navratri week.",
		})
	}

	if d, ok := variableDates["Diwali"]; ok {
		festivals = append(festivals, domain.FestivalCalendar{
			FestivalName:          "Diwali",
			Year:                  year,
			EventType:             "festival",
			StartDate:             d.AddDate(0, 0, -10), // 10-day Diwali sale window
			EndDate:               d.AddDate(0, 0, 2),   // including Govardhan Puja / Bhai Dooj
			IsDateFixed:           false,
			ExpectedUpliftPercent: 60.0,
			Notes:                 "Diwali / Deepawali (variable date). Peak Indian D2C sales event. 10-day pre-event + 2 post-event days.",
		})
	}

	if d, ok := variableDates["Dhanteras"]; ok {
		festivals = append(festivals, domain.FestivalCalendar{
			FestivalName:          "Dhanteras",
			Year:                  year,
			EventType:             "festival",
			StartDate:             d.AddDate(0, 0, -2),
			EndDate:               d,
			IsDateFixed:           false,
			ExpectedUpliftPercent: 45.0,
			Notes:                 "Dhanteras — two days before Diwali. High electronics/jewellery/home uplift.",
		})
	}

	return festivals
}

// variableDateMap returns the main festival date (the festival day itself, not the sale window)
// for each variable-date festival in the given year.
//
// SOURCE: Computed from the traditional lunisolar Panchang calendar.
// These dates must be reviewed and updated when almanac data beyond 2027 is needed.
//
// Key: festival name, Value: the primary festival date in UTC midnight.
func variableDateMap(year int) map[string]time.Time {
	dates := map[string]map[string]time.Time{
		// Holi: full moon of Phalguna (Holika Dahan eve)
		"2025": {
			"Holi":     date(2025, time.March, 14),
			"Dussehra": date(2025, time.October, 2),
			"Diwali":   date(2025, time.October, 20),
			"Dhanteras": date(2025, time.October, 18),
		},
		"2026": {
			"Holi":     date(2026, time.March, 3),
			"Dussehra": date(2026, time.October, 22),
			"Diwali":   date(2026, time.November, 8),
			"Dhanteras": date(2026, time.November, 6),
		},
		"2027": {
			"Holi":     date(2027, time.March, 22),
			"Dussehra": date(2027, time.October, 11),
			"Diwali":   date(2027, time.October, 29),
			"Dhanteras": date(2027, time.October, 27),
		},
	}

	key := fmt.Sprintf("%d", year)
	if m, ok := dates[key]; ok {
		return m
	}

	// For years without seed data, return an empty map.
	// Operators must manually insert via /v1/admin/festival-calendar or re-seed.
	slog.Warn("festival seeder: no variable-date festival data for year — manual seeding required", "year", year)
	return map[string]time.Time{}
}

// date returns a UTC time.Time set to midnight of the given y/m/d.
func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
