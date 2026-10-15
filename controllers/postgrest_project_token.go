package controllers

// miniship (miniship-cloud#1428): a token the install signed opens one
// Project on the SDK's /rest/v1 address, the one its project claim names.
//
// On miniship cloud the gateway's Check swaps a Project's key for a token it
// signs with the install's key (ES256, MINISHIP_JWT_KEYS): role anon for the
// publishable key, service_role for the secret key, and a project claim
// naming the Project the key belongs to. Patch 18's token check verifies it
// against the install's JWKS, which holds the same key for every Project, so
// the signature alone says nothing about which Project the token is for. A
// service_role token of Project A is as good a signature on Project B's call
// as on A's. What says where a token is worth anything is its project claim,
// held here to the Project the call is for.
//
// The Project the call is for is PROJECT_HEADER's, which the gateway writes:
// it removes a caller's own copy before any filter reads the request (its
// early header mutation, GATEWAY_HEADERS) and the Check sets it from the
// address the call arrived at. So a caller can name a Project only by calling
// at that Project's address, and the Check takes a key there only when it is
// that Project's. The claim and the header are two things the Check wrote
// about one call, and a token carried from another call names another
// Project.
//
// A token with no project claim is refused too where PROJECT_HEADER is set.
// The install's own tokens (the api service token, the anon token) name no
// Project and are worth every Project's rows; nothing on cloud sends them to
// /rest/v1, and an address that names its Project in a header is not one they
// should open.
//
// A self-host install sets no PROJECT_HEADER. It serves one Project, and the
// install's tokens are that Project's, so nothing here is asked and the
// install's tokens read as they did.

import "encoding/json"

// ProjectClaim is the claim the Check's token names its Project in: the cloud
// api's PROJECT_TOKEN_PROJECT_CLAIM.
const ProjectClaim = "project"

// checkTokenProject refuses an install-signed token whose project claim is
// not the Project the call is for, where the gateway names that Project.
func (h *CRUDHandler) checkTokenProject(claims []byte, project string) error {
	if h.projectHeader == "" {
		return nil
	}
	var named map[string]any
	if err := json.Unmarshal(claims, &named); err != nil {
		return pgrTokenRefused("the token's claims could not be read")
	}
	switch claimed, _ := named[ProjectClaim].(string); {
	case claimed == "":
		return pgrTokenRefused("the token names no Project, and this address is one Project's")
	case claimed != project:
		return pgrTokenRefused("the token was issued for another Project")
	}
	return nil
}
