package billing

import (
	"strings"
)

func DetectPaymentMethod(platform string, rawPayload map[string]interface{}) string {
	platform = strings.ToLower(platform)
	switch platform {
	case "shopify":
		// Primary: payment_gateway field
		gatewayRaw, _ := rawPayload["payment_gateway"].(string)
		gateway := strings.ToLower(strings.TrimSpace(gatewayRaw))

		// COD indicators (case-insensitive substring match, in priority order):
		if strings.Contains(gateway, "cash_on_delivery") ||
			strings.Contains(gateway, "cod") ||
			strings.Contains(gateway, "cash on delivery") ||
			strings.Contains(gateway, "manual") {
			return "cod"
		}

		// Prepaid indicators:
		prepaidKeywords := []string{"razorpay", "payu", "cashfree", "stripe", "paypal", "upi", "phonepe", "gpay", "amazon_pay"}
		for _, kw := range prepaidKeywords {
			if strings.Contains(gateway, kw) {
				return "prepaid"
			}
		}

		// Secondary check: if payment_gateway is ambiguous, check financial_status:
		finStatusRaw, _ := rawPayload["financial_status"].(string)
		finStatus := strings.ToLower(strings.TrimSpace(finStatusRaw))
		switch finStatus {
		case "paid":
			return "prepaid"
		case "pending":
			return "cod"
		case "authorized":
			return "prepaid"
		}

		// Default to prepaid if gateway is present but not matched as COD
		if gateway != "" {
			return "prepaid"
		}
		return "unknown"

	case "woocommerce":
		// Primary: payment_method field (NOT payment_method_title which is display text)
		methodRaw, _ := rawPayload["payment_method"].(string)
		method := strings.ToLower(strings.TrimSpace(methodRaw))
		titleRaw, _ := rawPayload["payment_method_title"].(string)
		title := strings.ToLower(strings.TrimSpace(titleRaw))

		// COD indicators: "cod" exactly, or payment_method_title containing "cash"
		if method == "cod" || strings.Contains(title, "cash") {
			return "cod"
		}

		// Prepaid indicators:
		prepaidKeywords := []string{"razorpay", "payu", "cashfree", "wc_stripe", "paypal", "upi_wc"}
		for _, kw := range prepaidKeywords {
			if strings.Contains(method, kw) {
				return "prepaid"
			}
		}

		// Secondary: order status
		statusRaw, _ := rawPayload["status"].(string)
		status := strings.ToLower(strings.TrimSpace(statusRaw))
		if status == "processing" && (method == "cod" || strings.Contains(title, "cash")) {
			return "cod"
		}
		if status == "completed" {
			if method == "cod" || strings.Contains(title, "cash") {
				return "cod"
			}
			return "prepaid"
		}

		if method != "" {
			return "prepaid"
		}
		return "unknown"

	case "magento":
		// Primary: payment.method field in order object
		var method string
		if paymentObj, ok := rawPayload["payment"].(map[string]interface{}); ok {
			methodRaw, _ := paymentObj["method"].(string)
			method = strings.ToLower(strings.TrimSpace(methodRaw))
		}

		// COD indicators: "cashondelivery" exactly, "free" (sometimes used for COD with custom plugins)
		if method == "cashondelivery" || method == "free" {
			return "cod"
		}

		// Prepaid indicators:
		prepaidKeywords := []string{"razorpay", "payu", "stripe", "paypal_express"}
		for _, kw := range prepaidKeywords {
			if strings.Contains(method, kw) {
				return "prepaid"
			}
		}

		if method != "" {
			return "prepaid"
		}
		return "unknown"

	default: // custom/generic
		// Look for a "payment_method" key anywhere in the top-level payload
		methodRaw, _ := rawPayload["payment_method"].(string)
		method := strings.ToLower(strings.TrimSpace(methodRaw))

		if method == "cod" || strings.Contains(method, "cash") {
			return "cod"
		}
		prepaidKeywords := []string{"razorpay", "payu", "cashfree", "stripe", "paypal", "upi", "phonepe", "gpay", "amazon_pay"}
		for _, kw := range prepaidKeywords {
			if strings.Contains(method, kw) {
				return "prepaid"
			}
		}

		if method != "" {
			return "prepaid"
		}
		return "unknown"
	}
}
