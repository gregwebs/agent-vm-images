package claude

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// clockZones are the two extreme zones, which differ from UTC in opposite directions.
// Kiritimati is UTC+14, so its local date runs ahead of the UTC date from 10:00 UTC;
// Etc/GMT+12 is UTC-12, so its local date lags from 00:00 UTC. Every hour of the UTC
// day is therefore covered by at least one of the two, which is what makes the pair
// time-of-day independent instead of a coin flip.
var clockZones = []string{"Pacific/Kiritimati", "Etc/GMT+12"}

// utcToday is the UTC date, which is the only date the pin has.
func utcToday() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// atAStableUTCDate runs call unless the UTC date rolls over mid-case, then retries
// once.
//
// Every case here derives a pin date from "the UTC date right now". A case that
// straddles UTC midnight would compare against a date the process no longer agrees
// with, so a genuine rollover is retried rather than reported -- but only when the
// date really moved, never to excuse a mutant. call reports its behavioural
// assertions as errors rather than stopping the test, which is what makes the retry
// possible without catching a panic; a fixture defect still stops the case.
func atAStableUTCDate(t *testing.T, what string, call func(t *testing.T, today time.Time) error) {
	t.Helper()
	for attempt := 0; attempt < 2; attempt++ {
		today := utcToday()
		err := call(t, today)
		if utcToday().Equal(today) {
			// The date held still, so this attempt's answer stands: report its failure,
			// or accept it.
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			return
		}
		// The date moved under the case. A pass is no more trustworthy than a failure
		// here -- it compared against a date the process no longer agrees with -- so
		// both are retried, once.
	}
	t.Fatalf("%s: the UTC date rolled over twice while this case ran", what)
}

// TestClockCLIAcceptsUTCTodayAndRefusesUTCTomorrowInEitherExtremeZone: the real CLI
// accepts UTC today and refuses UTC tomorrow, in both zones.
//
// This is the future-date half of the regression, through real processes: a checker
// that compared with a local date cannot satisfy both assertions in both zones at
// once. Where a zone's local date is a day ahead of UTC it accepts the pin dated UTC
// tomorrow; where it is a day behind it refuses the pin dated UTC today. At every hour
// of the UTC day at least one zone is in one of those states, so no wall clock lets
// that mutant through.
func TestClockCLIAcceptsUTCTodayAndRefusesUTCTomorrowInEitherExtremeZone(t *testing.T) {
	fixture := testFixture(t)
	atAStableUTCDate(t, "either extreme zone", func(t *testing.T, today time.Time) error {
		for _, zone := range clockZones {
			for _, row := range []struct {
				name     string
				day      time.Time
				accepted bool
			}{
				{name: "utc-today", day: today, accepted: true},
				{name: "utc-tomorrow", day: today.AddDate(0, 0, 1), accepted: false},
			} {
				day := row.day.Format(time.DateOnly)
				directory := fixture.mutate(t, "zone-"+fileName(zone)+"-"+row.name,
					func(pin map[string]any) {
						object(t, pin, "authentication")["verified_on"] = day
					})
				result := runReader(t, pinInvocation("check", directory, "--version", Version),
					map[string]string{"TZ": zone})
				if row.accepted {
					if result.Status != 0 {
						return fmt.Errorf("under TZ=%s the pin dated %s was refused: %s",
							zone, day, result.Report())
					}
					continue
				}
				text, err := refusalText(result, []string{"is in the future"})
				if err != nil {
					return fmt.Errorf("under TZ=%s the pin dated %s: %w", zone, day, err)
				}
				if !strings.Contains(text, day) {
					return fmt.Errorf("under TZ=%s the refusal of %s did not name the date: %s",
						zone, day, text)
				}
			}
		}
		return nil
	})
}

// TestClockWriterStampsTheUTCDateAndTheCheckerAcceptsItFromEitherZone: the real writer
// stamps the UTC date, and the real reader accepts it.
//
// This is the end-to-end half of the regression: the writer runs on a maintainer's
// host, east or west of UTC, and the bytes it publishes are then checked on builders
// elsewhere. Both sides are real processes here -- only the upstream bytes are served
// from the fixture -- and the pair is run in each extreme zone. The stamp itself is
// asserted on its own, so a writer that falls back to the host's local date is caught
// in whichever zone's date differs, and the published closure is then checked from
// both zones, so a stamp ahead of UTC cannot pass just because it was written where it
// looked like today.
func TestClockWriterStampsTheUTCDateAndTheCheckerAcceptsItFromEitherZone(t *testing.T) {
	atAStableUTCDate(t, "the writer's stamp", func(t *testing.T, today time.Time) error {
		for _, zone := range clockZones {
			repo := newWriterRepo(t, scratch(t, "zone-writer-"+fileName(zone)),
				unadopted(), dockerfileAt("0.0.0"))
			result := repo.run(t, flags("--version", Version), withEnv("TZ", zone))
			if result.Status != 0 {
				return fmt.Errorf("the writer under TZ=%s exited %d\n%s", zone, result.Status,
					result.Report())
			}
			stamped, ok := object(t, repo.pin(t), "authentication")["verified_on"].(string)
			if !ok {
				return fmt.Errorf("under TZ=%s the writer stamped no verified_on date", zone)
			}
			if stamped != today.Format(time.DateOnly) {
				return fmt.Errorf("under TZ=%s the writer stamped verified_on %s, not the UTC date %s",
					zone, stamped, today.Format(time.DateOnly))
			}
			published := repo.root + "/" + pinDirectory
			for _, readerZone := range clockZones {
				checked := runReader(t, pinInvocation("check", published, "--version", Version),
					map[string]string{"TZ": readerZone})
				if checked.Status != 0 {
					return fmt.Errorf("the closure written under TZ=%s was refused under TZ=%s\n%s",
						zone, readerZone, checked.Report())
				}
			}
		}
		return nil
	})
}

// TestClockClosureVerifiesUnderAFarEastAndFarWestTZ: the same closure verifies with the
// ambient clock far east and far west.
func TestClockClosureVerifiesUnderAFarEastAndFarWestTZ(t *testing.T) {
	fixture := testFixture(t)
	for _, zone := range clockZones {
		t.Run(zone, func(t *testing.T) {
			printed := strings.TrimSpace(accepts(t,
				pinInvocation("version", fixture.closure), inZone(zone)).Stdout)
			if printed != Version {
				t.Fatalf("under TZ=%s version printed %q", zone, printed)
			}
		})
	}
}
