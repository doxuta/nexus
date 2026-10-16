// Shim: the toast system is the SLDS one in components/slds/Toast.tsx. Kept so the ~50 call sites
// (`import { toast } from "@/components/ui/Toaster"`) and tests that mock this path stay unchanged.
export { Toaster, toast } from "@/components/slds/Toast";
