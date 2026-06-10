package live

func FormatVersion(providerVersion, protocolDigest string) string {
	if providerVersion == "" {
		return "protocol-schema/" + protocolDigest
	}
	if protocolDigest == "" {
		return providerVersion
	}
	return providerVersion + " protocol-schema/" + protocolDigest
}
