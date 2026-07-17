package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/arpitmandhotra/api-integrator/internal/database"
	"github.com/arpitmandhotra/api-integrator/internal/domain"
	"github.com/joho/godotenv"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type PincodeAccumulator struct {
	Pincode      string
	StateName    string
	District     string
	RegionName   string
	CircleName   string
	DivisionName string
	OfficeName   string
	OfficeType   string
	IsDelivery   bool
	SumLat       float64
	SumLng       float64
	CountLat     int
	CountLng     int
	Priority     int // 3 = HO, 2 = Delivery, 1 = Any/First seen
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Printf("Warning: .env file not found: %v", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	db := database.NewPostgresClient()

	var count int64
	if err := db.Model(&domain.PincodeReference{}).Count(&count).Error; err == nil && count > 0 {
		slog.Info("PincodeReference table is already seeded. Skipping seed job.")
		return
	}

	tmpFile := "pincode_temp.csv"
	primaryURL := "https://data.gov.in/files/ogdpv2dms/s3fs-public/dataurl03122020/pincode.csv"
	fallbackURL := "https://raw.githubusercontent.com/saravanakumargn/All-India-Pincode-Directory/master/pincode.csv"

	slog.Info("downloading pincode CSV...")
	err := downloadFile(tmpFile, primaryURL)
	if err != nil {
		slog.Warn("primary download failed, trying fallback...", "error", err)
		err = downloadFile(tmpFile, fallbackURL)
		if err != nil {
			slog.Error("fallback download failed", "error", err)
			os.Exit(1)
		}
	}
	defer os.Remove(tmpFile)

	slog.Info("parsing pincode CSV...")
	accumulators, err := parseCSV(tmpFile)
	if err != nil {
		slog.Error("failed to parse CSV", "error", err)
		os.Exit(1)
	}

	slog.Info("converting accumulators to PincodeReference records...")
	now := time.Now().UTC()
	var records []domain.PincodeReference
	for _, acc := range accumulators {
		var avgLat, avgLng float64
		hasCoords := false
		if acc.CountLat > 0 && acc.CountLng > 0 {
			avgLat = acc.SumLat / float64(acc.CountLat)
			avgLng = acc.SumLng / float64(acc.CountLng)
			if avgLat >= 6.0 && avgLat <= 38.0 && avgLng >= 68.0 && avgLng <= 98.0 {
				hasCoords = true
			} else {
				avgLat = 0.0
				avgLng = 0.0
			}
		}

		geoTier := classifyGeoTier(acc.District, acc.StateName)

		records = append(records, domain.PincodeReference{
			Pincode:        acc.Pincode,
			StateName:      acc.StateName,
			District:       acc.District,
			RegionName:     acc.RegionName,
			CircleName:     acc.CircleName,
			DivisionName:   acc.DivisionName,
			OfficeName:     acc.OfficeName,
			OfficeType:     acc.OfficeType,
			IsDelivery:     acc.IsDelivery,
			Latitude:       avgLat,
			Longitude:      avgLng,
			HasCoordinates: hasCoords,
			GeoTier:        geoTier,
			LastSyncedAt:   now,
		})
	}

	slog.Info("upserting pincode reference records into database...", "count", len(records))
	err = db.Transaction(func(tx *gorm.DB) error {
		return tx.Clauses(clause.OnConflict{
			UpdateAll: true,
		}).CreateInBatches(records, 500).Error
	})
	if err != nil {
		slog.Error("failed to upsert pincode reference records", "error", err)
		os.Exit(1)
	}

	slog.Info("pincode seeding completed successfully.")
}

func downloadFile(filepath string, url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}

	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

func parseCSV(filepath string) (map[string]*PincodeAccumulator, error) {
	file, err := os.Open(filepath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	reader := csv.NewReader(file)
	// Some government datasets contain irregular column lengths or fields. Let's allow variable fields.
	reader.FieldsPerRecord = -1

	// Skip header
	header, err := reader.Read()
	if err != nil {
		return nil, err
	}

	// Verify columns mapping: CircleName, RegionName, DivisionName, OfficeName, Pincode, OfficeType, Delivery, District, StateName, Latitude, Longitude
	// Let's print headers to help debugging
	slog.Info("CSV headers", "columns", header)

	accumulators := make(map[string]*PincodeAccumulator)
	rowCount := 0

	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Skip malformed row
			continue
		}

		if len(record) < 9 {
			continue
		}

		circleName := strings.TrimSpace(record[0])
		regionName := strings.TrimSpace(record[1])
		divisionName := strings.TrimSpace(record[2])
		officeName := strings.TrimSpace(record[3])
		pincode := strings.TrimSpace(record[4])
		officeType := strings.TrimSpace(record[5])
		delivery := strings.TrimSpace(record[6])
		district := strings.TrimSpace(record[7])
		stateName := strings.TrimSpace(record[8])

		// Validate 6 digit numeric pincode
		if len(pincode) != 6 {
			continue
		}
		if _, err := strconv.Atoi(pincode); err != nil {
			continue
		}

		var latVal, lngVal float64
		hasCoords := false
		if len(record) >= 11 {
			latStr := strings.TrimSpace(record[9])
			lngStr := strings.TrimSpace(record[10])
			if latStr != "" && lngStr != "" {
				lat, err1 := strconv.ParseFloat(latStr, 64)
				lng, err2 := strconv.ParseFloat(lngStr, 64)
				if err1 == nil && err2 == nil && lat != 0.0 && lng != 0.0 {
					latVal = lat
					lngVal = lng
					hasCoords = true
				}
			}
		}

		priority := 1
		isHO := strings.ToUpper(officeType) == "HO"
		isDelivery := strings.EqualFold(delivery, "delivery")

		if isHO {
			priority = 3
		} else if isDelivery {
			priority = 2
		}

		existing, found := accumulators[pincode]
		if !found {
			acc := &PincodeAccumulator{
				Pincode:      pincode,
				StateName:    stateName,
				District:     district,
				RegionName:   regionName,
				CircleName:   circleName,
				DivisionName: divisionName,
				OfficeName:   officeName,
				OfficeType:   officeType,
				IsDelivery:   isDelivery,
				Priority:     priority,
			}
			if hasCoords {
				acc.SumLat = latVal
				acc.SumLng = lngVal
				acc.CountLat = 1
				acc.CountLng = 1
			}
			accumulators[pincode] = acc
		} else {
			if hasCoords {
				existing.SumLat += latVal
				existing.SumLng += lngVal
				existing.CountLat++
				existing.CountLng++
			}
			if priority > existing.Priority {
				existing.StateName = stateName
				existing.District = district
				existing.RegionName = regionName
				existing.CircleName = circleName
				existing.DivisionName = divisionName
				existing.OfficeName = officeName
				existing.OfficeType = officeType
				existing.IsDelivery = isDelivery
				existing.Priority = priority
			}
		}
		rowCount++
	}

	slog.Info("CSV parsing complete", "parsed_rows", rowCount, "unique_pincodes", len(accumulators))
	return accumulators, nil
}

func classifyGeoTier(district, state string) string {
	d := strings.ToLower(strings.TrimSpace(district))
	s := strings.ToLower(strings.TrimSpace(state))

	// Metro check
	// Districts part of Mumbai, Pune, Delhi, Gurugram, Noida, Bengaluru, Hyderabad, Chennai, Kolkata, Ahmedabad, Surat
	metroDistricts := map[string]bool{
		"mumbai":               true,
		"mumbai suburban":      true,
		"pune":                 true,
		"delhi":                true,
		"new delhi":            true,
		"north delhi":          true,
		"south delhi":          true,
		"east delhi":           true,
		"west delhi":           true,
		"north east delhi":     true,
		"north west delhi":     true,
		"south east delhi":     true,
		"south west delhi":     true,
		"central delhi":        true,
		"gurugram":             true,
		"gurgaon":              true,
		"noida":                true,
		"gautam buddha nagar":  true,
		"gautam budh nagar":    true,
		"bengaluru":            true,
		"bengaluru urban":      true,
		"bengaluru rural":      true,
		"bangalore":            true,
		"bangalore urban":      true,
		"bangalore rural":      true,
		"hyderabad":            true,
		"chennai":              true,
		"kolkata":              true,
		"ahmedabad":            true,
		"surat":                true,
	}

	if metroDistricts[d] {
		return "METRO"
	}

	// State name sub-matches for NCT of Delhi
	if strings.Contains(s, "delhi") {
		return "METRO"
	}

	// Tier 2 check
	tier2Districts := map[string]bool{
		"jaipur":             true,
		"lucknow":            true,
		"kanpur":             true,
		"kanpur nagar":       true,
		"kanpur dehat":       true,
		"nagpur":             true,
		"indore":             true,
		"bhopal":             true,
		"patna":              true,
		"vadodara":           true,
		"coimbatore":         true,
		"visakhapatnam":      true,
		"agra":               true,
		"nashik":             true,
		"nasik":              true,
		"meerut":             true,
		"faridabad":          true,
		"rajkot":             true,
		"varanasi":           true,
		"srinagar":           true,
		"aurangabad":         true,
		"dhanbad":            true,
		"amritsar":           true,
		"allahabad":          true,
		"prayagraj":          true,
		"ranchi":             true,
		"howrah":             true,
		"jodhpur":            true,
		"guwahati":           true,
		"chandigarh":         true,
		"thiruvananthapuram": true,
		"trivandrum":         true,
		"kochi":              true,
		"cochin":             true,
		"ernakulam":          true,
		"mysuru":             true,
		"mysore":             true,
		"hubli":              true,
		"dharwad":            true,
		"belgaum":            true,
		"belagavi":           true,
		"mangalu":            true,
		"mangaluru":          true,
		"mangalore":          true,
		"tiruchirappalli":    true,
		"trichy":             true,
		"salem":              true,
		"tirunelveli":        true,
		"madurai":            true,
		"raipur":             true,
		"bhubaneswar":        true,
		"khordha":            true, // Bhubaneswar is in Khordha district
		"cuttack":            true,
		"dehradun":           true,
		"jammu":              true,
		"ludhiana":           true,
		"ghaziabad":          true,
	}

	if tier2Districts[d] {
		return "TIER2"
	}

	return "TIER3"
}
