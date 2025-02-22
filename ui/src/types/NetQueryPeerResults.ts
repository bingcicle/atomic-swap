export type Currency = 'ETH' | 'XMR'

export interface OfferRaw {
    offerID: String
    provides: Currency
    minAmount: Number
    maxAmount: Number
    exchangeRate: Number
    version: String
    ethAsset: Currency
    nonce: Number
}

export interface NetQueryPeerResult {
    offers: OfferRaw[]
}