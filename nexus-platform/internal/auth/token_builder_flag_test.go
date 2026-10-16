package auth

// token_builder_flag_test.go — specs/063-draft-publish T009: the builder flag is never a claim. A
// forged access token whose payload names it (any spelling) still verifies as NOT a platform operator.

import (
	"reflect"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestVerifyAccessToken_IgnoresForgedBuilderFlag(t *testing.T) {
	svc := newTestService(t)
	now := time.Now()
	for _, key := range []string{"IsPlatformOperator", "isPlatformOperator", "isBuilder", "IsBuilder"} {
		t.Run(key, func(t *testing.T) {
			forged := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
				"uid": testUserID, "tid": testTenantID, "adm": true, "typ": string(TokenTypeAccess), key: true,
				"exp": now.Add(time.Minute).Unix(), "iat": now.Unix(),
			})
			tok, err := forged.SignedString([]byte(testSecret))
			if err != nil {
				t.Fatalf("sign: %v", err)
			}
			claims, err := svc.VerifyAccessToken(tok)
			if err != nil {
				t.Fatalf("VerifyAccessToken: %v", err)
			}
			if claims.IsPlatformOperator {
				t.Fatalf("payload key %q set Subject.IsPlatformOperator; the flag must only come from the live row", key)
			}
		})
	}
}

// Tie to the single-definition rule: no second field with the same meaning on Subject.
func TestSubjectHasNoIsBuilderField(t *testing.T) {
	if _, ok := reflect.TypeOf(Subject{}).FieldByName("IsBuilder"); ok {
		t.Fatal("Subject must not grow an IsBuilder field; metaversion.IsBuilder is the one definition")
	}
}
