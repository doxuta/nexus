"use client";

// SLDS picklist (blueprint `picklist`, SLDS 2.264.1): the select-only combobox markup with the
// container sized `slds-size_small`; multi-select shows "n mục" in the field and the choices as pills
// in the selection group below. Behaviour is Combobox's (select-only keyboard, Radix placement).
import clsx from "clsx";
import { Combobox, type ComboboxProps } from "./Combobox";

/** Combobox props; `variant` is always "select" here. */
export function Picklist(props: ComboboxProps) {
  return <Combobox {...props} variant="select" containerClassName={clsx("slds-size_small", props.containerClassName)} />;
}
