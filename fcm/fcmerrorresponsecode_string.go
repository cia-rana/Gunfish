// Code generated by "stringer -type FCMErrorResponseCode error.go"; DO NOT EDIT

package fcm

import "fmt"

const _FCMErrorResponseCode_name = "MissingRegistrationInvalidRegistrationNotRegisteredInvalidPackageNameMismatchSenderIdMessageTooBigInvalidDataKeyInvalidTtlDeviceMessageRateExceededTopicsMessageRateExceededInvalidApnsCredentialsAuthenticationErrorInvalidJSONUnavailableInternalServerErrorUnknownError"

var _FCMErrorResponseCode_index = [...]uint16{0, 19, 38, 51, 69, 85, 98, 112, 122, 147, 172, 194, 213, 224, 235, 254, 266}

func (i FCMErrorResponseCode) String() string {
	if i < 0 || i >= FCMErrorResponseCode(len(_FCMErrorResponseCode_index)-1) {
		return fmt.Sprintf("FCMErrorResponseCode(%d)", i)
	}
	return _FCMErrorResponseCode_name[_FCMErrorResponseCode_index[i]:_FCMErrorResponseCode_index[i+1]]
}
