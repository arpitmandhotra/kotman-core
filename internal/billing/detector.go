package billing

import (
	"log/slog"
	"strings"

	"github.com/arpitmandhotra/api-integrator/internal/domain"
)

type CheckoutDetectionResult struct {
	Mode           string // "native" | "third_party"
	ThirdPartyName string // "" | "gokwik" | "shopflo" | "razorpay_magic" | "unknown_third_party"
	Confidence     string // "high" | "medium" | "low"
	DetectionPath  string // which signal fired: "cart_attribute" | "metadata_fingerprint" | "merchant_config" | "default_fallback"
	RequiresReview bool
}

type OrderPayload struct {
	PlatformOrderID string
	OrderValuePaise int
	PaymentMethod   string
	PhoneRaw        string
	Platform        string
	NoteAttributes  map[string]string
	Tags            []string
	SourceName      string
	RawJSON         string
}

func DetectCheckoutMode(payload OrderPayload, merchantSettings domain.MerchantSettings) CheckoutDetectionResult {
	// SIGNAL 1 — Cart attribute (Shopify native flow only):
	hasKaughtmanRisk := false
	hasKaughtmanTs := false
	for k, v := range payload.NoteAttributes {
		kLower := strings.ToLower(k)
		if kLower == "kaughtman_risk" && (v == "high" || v == "low") {
			hasKaughtmanRisk = true
		}
		if kLower == "kaughtman_ts" && v != "" {
			hasKaughtmanTs = true
		}
	}

	if hasKaughtmanRisk || hasKaughtmanTs {
		res := CheckoutDetectionResult{
			Mode:           "native",
			ThirdPartyName: "",
			Confidence:     "high",
			DetectionPath:  "cart_attribute",
			RequiresReview: false,
		}
		logResult(payload, res)
		return res
	}

	// SIGNAL 2 — Known third-party checkout fingerprints in order metadata:
	srcLower := strings.ToLower(payload.SourceName)

	// GoKwik fingerprints:
	hasGoKwikAttribute := false
	for k := range payload.NoteAttributes {
		kLower := strings.ToLower(k)
		if kLower == "gokwik_order_id" || kLower == "gk_order_id" || kLower == "kwik_order_id" ||
			kLower == "gokwik_risk_flag" || kLower == "gk_risk" || strings.Contains(kLower, "gokwik") {
			hasGoKwikAttribute = true
			break
		}
	}
	hasGoKwikTag := false
	for _, tag := range payload.Tags {
		if strings.Contains(strings.ToLower(tag), "gokwik") {
			hasGoKwikTag = true
			break
		}
	}
	isGoKwikSource := srcLower == "gokwik" || srcLower == "kwik"

	if hasGoKwikAttribute || hasGoKwikTag || isGoKwikSource {
		res := CheckoutDetectionResult{
			Mode:           "third_party",
			ThirdPartyName: "gokwik",
			Confidence:     "high",
			DetectionPath:  "metadata_fingerprint",
			RequiresReview: false,
		}
		logResult(payload, res)
		return res
	}

	// Shopflo fingerprints:
	hasShopfloAttribute := false
	for k := range payload.NoteAttributes {
		kLower := strings.ToLower(k)
		if kLower == "shopflo_order_id" || kLower == "sf_checkout_id" {
			hasShopfloAttribute = true
			break
		}
	}
	isShopfloSource := srcLower == "shopflo"

	if hasShopfloAttribute || isShopfloSource {
		res := CheckoutDetectionResult{
			Mode:           "third_party",
			ThirdPartyName: "shopflo",
			Confidence:     "high",
			DetectionPath:  "metadata_fingerprint",
			RequiresReview: false,
		}
		logResult(payload, res)
		return res
	}

	// Razorpay Magic Checkout fingerprints:
	hasRazorpayMagicAttribute := false
	for k := range payload.NoteAttributes {
		kLower := strings.ToLower(k)
		if kLower == "razorpay_magic_checkout" || kLower == "rzp_checkout_id" {
			hasRazorpayMagicAttribute = true
			break
		}
	}
	isRazorpayMagicSource := srcLower == "razorpay magic checkout" || srcLower == "razorpay_magic" || srcLower == "razorpay_magic_checkout"

	if hasRazorpayMagicAttribute || isRazorpayMagicSource {
		res := CheckoutDetectionResult{
			Mode:           "third_party",
			ThirdPartyName: "razorpay_magic",
			Confidence:     "high",
			DetectionPath:  "metadata_fingerprint",
			RequiresReview: false,
		}
		logResult(payload, res)
		return res
	}

	// SIGNAL 3 — Kaughtman SDK absence + known COD gateway:
	if payload.PaymentMethod == "cod" {
		switch merchantSettings.CheckoutMode {
		case "native":
			res := CheckoutDetectionResult{
				Mode:           "native",
				ThirdPartyName: "",
				Confidence:     "medium",
				DetectionPath:  "merchant_config",
				RequiresReview: false,
			}
			logResult(payload, res)
			return res
		case "third_party":
			tpName := merchantSettings.ThirdPartyCheckout
			if tpName == "" {
				tpName = "unknown_third_party"
			}
			res := CheckoutDetectionResult{
				Mode:           "third_party",
				ThirdPartyName: tpName,
				Confidence:     "medium",
				DetectionPath:  "merchant_config",
				RequiresReview: false,
			}
			logResult(payload, res)
			return res
		default: // "" or unconfigured
			res := CheckoutDetectionResult{
				Mode:           "native",
				ThirdPartyName: "",
				Confidence:     "low",
				DetectionPath:  "default_fallback",
				RequiresReview: true,
			}
			logResult(payload, res)
			return res
		}
	}

	// If payment_method is prepaid AND checkout_mode cannot be determined:
	res := CheckoutDetectionResult{
		Mode:           "native",
		ThirdPartyName: "",
		Confidence:     "low",
		DetectionPath:  "default_fallback",
		RequiresReview: true,
	}
	logResult(payload, res)
	return res
}

func logResult(payload OrderPayload, res CheckoutDetectionResult) {
	slog.Info("checkout mode detected",
		"order_id", payload.PlatformOrderID,
		"platform", payload.Platform,
		"detected_mode", res.Mode,
		"third_party_name", res.ThirdPartyName,
		"confidence", res.Confidence,
		"detection_path", res.DetectionPath,
		"requires_review", res.RequiresReview,
	)
}
