package v1alpha1

// PrivateCarrierLimit requests strict bounded decoding from a Secret driver
// (D384). A carrier that does not decode, does not validate or is larger than
// MaxObjectBytes is an error on Get and List, never a skipped row: a missing
// ledger, operation, budget, delivery, cursor or source carrier would otherwise
// read as absent and let a writer recreate or forget state it never saw.
func (*IntegrationLedger) PrivateCarrierLimit() int { return MaxObjectBytes }

// PrivateCarrierLimit requests strict bounded decoding (D384).
func (*IntegrationOperation) PrivateCarrierLimit() int { return MaxObjectBytes }

// PrivateCarrierLimit requests strict bounded decoding (D384).
func (*IntegrationBudget) PrivateCarrierLimit() int { return MaxObjectBytes }

// PrivateCarrierLimit requests strict bounded decoding (D384).
func (*IntegrationDelivery) PrivateCarrierLimit() int { return MaxObjectBytes }

// PrivateCarrierLimit requests strict bounded decoding (D384).
func (*IntegrationEventCursor) PrivateCarrierLimit() int { return MaxObjectBytes }

// PrivateCarrierLimit requests strict bounded decoding (D384).
func (*IntegrationSourceHead) PrivateCarrierLimit() int { return MaxObjectBytes }

// PrivateCarrierLimit requests strict bounded decoding (D384). A chunk holds
// at most MaxSourceChunkBytes of data, which stays below this bound after
// base64 and metadata.
func (*IntegrationSourceChunk) PrivateCarrierLimit() int { return MaxObjectBytes }

// PrivateCarrierLimit requests strict bounded decoding (D384).
func (*IntegrationNameAllocation) PrivateCarrierLimit() int { return MaxObjectBytes }
