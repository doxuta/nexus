import type { NextRequest } from "next/server";
import { devProxy } from "@/lib/devProxy";

// DEV-ONLY API proxy to DEV_API_PROXY_TARGET: lib/devProxy.ts says why and when it answers.

type Ctx = { params: Promise<{ path: string[] }> };

async function proxy(req: NextRequest, { params }: Ctx) {
  const { path } = await params;
  return devProxy(req, `/api/v1/${path.join("/")}`);
}

export const GET = proxy;
export const POST = proxy;
export const PUT = proxy;
export const PATCH = proxy;
export const DELETE = proxy;
