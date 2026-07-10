package classification

import (
	"strings"
	"unicode"
)

// geoEntry holds the normalized state name and its tier.
type geoEntry struct {
	State string
	Tier  int
}

// geoTierMap maps lowercase province strings (including common Shopify variants)
// to their normalized state name and geo tier.
var geoTierMap = map[string]geoEntry{
	// --- Tier 1: Major metros ---
	"maharashtra":    {State: "Maharashtra", Tier: 1},
	"delhi":          {State: "Delhi", Tier: 1},
	"new delhi":      {State: "Delhi", Tier: 1},
	"nct of delhi":   {State: "Delhi", Tier: 1},
	"ncr":            {State: "Delhi", Tier: 1},
	"karnataka":      {State: "Karnataka", Tier: 1},
	"tamil nadu":     {State: "Tamil Nadu", Tier: 1},
	"tamilnadu":      {State: "Tamil Nadu", Tier: 1},
	"telangana":      {State: "Telangana", Tier: 1},
	"gujarat":        {State: "Gujarat", Tier: 1},
	"west bengal":    {State: "West Bengal", Tier: 1},
	"westbengal":     {State: "West Bengal", Tier: 1},

	// --- Tier 2 ---
	"rajasthan":       {State: "Rajasthan", Tier: 2},
	"madhya pradesh":  {State: "Madhya Pradesh", Tier: 2},
	"madhyapradesh":   {State: "Madhya Pradesh", Tier: 2},
	"uttar pradesh":   {State: "Uttar Pradesh", Tier: 2},
	"uttarpradesh":    {State: "Uttar Pradesh", Tier: 2},
	"punjab":          {State: "Punjab", Tier: 2},
	"haryana":         {State: "Haryana", Tier: 2},
	"kerala":          {State: "Kerala", Tier: 2},
	"andhra pradesh":  {State: "Andhra Pradesh", Tier: 2},
	"andhrapradesh":   {State: "Andhra Pradesh", Tier: 2},
	"odisha":          {State: "Odisha", Tier: 2},
	"orissa":          {State: "Odisha", Tier: 2},
	"jharkhand":       {State: "Jharkhand", Tier: 2},
	"bihar":           {State: "Bihar", Tier: 2},
	"assam":           {State: "Assam", Tier: 2},
	"uttarakhand":     {State: "Uttarakhand", Tier: 2},
	"uttaranchal":     {State: "Uttarakhand", Tier: 2},
	"chhattisgarh":    {State: "Chhattisgarh", Tier: 2},
	"chattisgarh":     {State: "Chhattisgarh", Tier: 2},
	"goa":             {State: "Goa", Tier: 2},
	"himachal pradesh": {State: "Himachal Pradesh", Tier: 2},
	"himachalpradesh":  {State: "Himachal Pradesh", Tier: 2},

	// --- Tier 3: Explicitly listed for completeness ---
	"meghalaya":                    {State: "Meghalaya", Tier: 3},
	"manipur":                     {State: "Manipur", Tier: 3},
	"mizoram":                     {State: "Mizoram", Tier: 3},
	"nagaland":                    {State: "Nagaland", Tier: 3},
	"tripura":                     {State: "Tripura", Tier: 3},
	"arunachal pradesh":           {State: "Arunachal Pradesh", Tier: 3},
	"arunachalpradesh":            {State: "Arunachal Pradesh", Tier: 3},
	"sikkim":                      {State: "Sikkim", Tier: 3},
	"jammu and kashmir":           {State: "Jammu and Kashmir", Tier: 3},
	"jammu & kashmir":             {State: "Jammu and Kashmir", Tier: 3},
	"j&k":                         {State: "Jammu and Kashmir", Tier: 3},
	"ladakh":                      {State: "Ladakh", Tier: 3},
	"puducherry":                  {State: "Puducherry", Tier: 3},
	"pondicherry":                 {State: "Puducherry", Tier: 3},
	"chandigarh":                  {State: "Chandigarh", Tier: 3},
	"daman and diu":               {State: "Daman and Diu", Tier: 3},
	"daman & diu":                 {State: "Daman and Diu", Tier: 3},
	"dadra and nagar haveli":      {State: "Dadra and Nagar Haveli", Tier: 3},
	"dadra & nagar haveli":        {State: "Dadra and Nagar Haveli", Tier: 3},
	"lakshadweep":                 {State: "Lakshadweep", Tier: 3},
	"andaman and nicobar islands": {State: "Andaman and Nicobar Islands", Tier: 3},
	"andaman & nicobar islands":   {State: "Andaman and Nicobar Islands", Tier: 3},
	"andaman and nicobar":         {State: "Andaman and Nicobar Islands", Tier: 3},
}

// LookupGeoTier maps an Indian state/province string to a normalized state name
// and a geo tier (1, 2, or 3). Unmapped provinces default to tier 3.
func LookupGeoTier(province string) (geoState string, geoTier int) {
	normalized := strings.ToLower(strings.TrimSpace(province))

	if entry, ok := geoTierMap[normalized]; ok {
		return entry.State, entry.Tier
	}

	// Default: title-case the input and return tier 3
	return titleCase(province), 3
}

// titleCase converts a string to title case (first letter of each word uppercase).
func titleCase(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}

	prev := ' '
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(rune(prev)) || prev == ' ' {
			prev = r
			return unicode.ToUpper(r)
		}
		prev = r
		return unicode.ToLower(r)
	}, s)
}
