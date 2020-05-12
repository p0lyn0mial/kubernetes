package failure_detector

// SimpleWeightedEndpointStatusEvaluator an external policy evaluator that sets the status and weight of the given endpoint based on the collected samples.
// It returns true only if status or wight have changed otherwise false
//
// WeightedEndpointStatus.Status:
// will be set to EndpointStatusReasonTooManyErrors only when it sees 10 errors
// otherwise it will be set to an empty string
//
// WeightedEndpointStatus.Weight:
// will be decreased by 0.1 for each encountered error for example:
//  - the value of 1 means no errors
//  - the value of 0 means it observed 10 errors
//  - the value of 0.7 means it observed 3 errors
func SimpleWeightedEndpointStatusEvaluator(endpoint *WeightedEndpointStatus) bool {
	errThreshold := 10
	errCount := 0

	for _, sample := range endpoint.data {
		if sample != nil && sample.err != nil {
			errCount++
		}
	}

	hasChanged := false
	if errCount >= errThreshold && endpoint.status != EndpointStatusReasonTooManyErrors {
		endpoint.status = EndpointStatusReasonTooManyErrors
		hasChanged = true
	} else if endpoint.status != "" {
		endpoint.status = ""
		hasChanged = true
	}

	newWeight := 1 - 0.1*float32(errCount)
	prevErrCount := weightToErrorCount(endpoint.weight)
	if prevErrCount != errCount {
		endpoint.weight = newWeight
		hasChanged = true
	}

	return hasChanged
}

func weightToErrorCount(weight float32) int {
	return int((1 - weight) * 10)
}
