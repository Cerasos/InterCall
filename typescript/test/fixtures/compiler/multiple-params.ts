import type { HandlerContext, Int32 } from "../../../src/index.js";

/** @intercall procedure */
export function add(
    _context: HandlerContext,
    left: Int32,
    right: Int32,
): Promise<Int32> {
    return Promise.resolve((left + right) as Int32);
}
