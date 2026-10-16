"use client";

// Vận hành › Việc tự động › Khoá kết nối (specs/078 lát u7-khoa-ket-noi, FR-077): the customer's own key vault, in the frame.
// Isolation (FR-135): while the token in hand does not belong to project S, NamedCredentials is not mounted at all, so no
// request is made for a customer the token is not for; the key on `slug` throws away the form, the reveal and the list on a switch.
import { NamedCredentials } from "@/components/screens/admin/NamedCredentials";
import { Spinner } from "@/components/slds/Spinner";
import { useScopeGate } from "@/components/studio/operate/useScopeGate";
import { useStudioIdentity } from "@/lib/studioContext";

export function CredentialsPage({ slug }: { slug: string }) {
  const identity = useStudioIdentity();
  const guard = useScopeGate(true);
  if (!identity.matches(slug)) {
    return (
      <div className="slds-is-relative slds-p-around_large">
        <Spinner size="small" />
      </div>
    );
  }
  return <NamedCredentials key={slug} guard={guard} />;
}
