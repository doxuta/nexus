import type { NextRequest } from "next/server";
import { devProxy } from "@/lib/devProxy";

// DEV-ONLY: `next dev` serves the endpoint the Studio prints for an agent (lib/admin.ts mcpEndpoint), so a
// contributor's `claude mcp add` line connects (issue #22). lib/devProxy.ts says why and when it answers.

const proxy = (req: NextRequest) => devProxy(req, "/mcp");

export const GET = proxy;
export const POST = proxy;
export const DELETE = proxy;
