export namespace compare {
	
	export class RunHarnessInfo {
	    harness: string;
	    status: string;
	    startedAt: string;
	    endedAt: string;
	    durationMs: number;
	    tokensIn: number;
	    tokensOut: number;
	    costUsd: number;
	    finalText: string;
	    error: string;
	    sessionId: string;
	
	    static createFrom(source: any = {}) {
	        return new RunHarnessInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.harness = source["harness"];
	        this.status = source["status"];
	        this.startedAt = source["startedAt"];
	        this.endedAt = source["endedAt"];
	        this.durationMs = source["durationMs"];
	        this.tokensIn = source["tokensIn"];
	        this.tokensOut = source["tokensOut"];
	        this.costUsd = source["costUsd"];
	        this.finalText = source["finalText"];
	        this.error = source["error"];
	        this.sessionId = source["sessionId"];
	    }
	}
	export class RunInfo {
	    id: number;
	    prompt: string;
	    harnesses: string[];
	    createdAt: string;
	    status: string;
	    active: boolean;
	    runners: RunHarnessInfo[];
	
	    static createFrom(source: any = {}) {
	        return new RunInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.prompt = source["prompt"];
	        this.harnesses = source["harnesses"];
	        this.createdAt = source["createdAt"];
	        this.status = source["status"];
	        this.active = source["active"];
	        this.runners = this.convertValues(source["runners"], RunHarnessInfo);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace explain {
	
	export class StepExplain {
	    summary: string;
	    from: string;
	    to: string;
	    visible: string;
	    why: string;
	    nextHint: string;
	    round: number;
	    toolKey: string;
	    callSeq: number;
	    resultSeq: number;
	    waitNote: string;
	
	    static createFrom(source: any = {}) {
	        return new StepExplain(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.summary = source["summary"];
	        this.from = source["from"];
	        this.to = source["to"];
	        this.visible = source["visible"];
	        this.why = source["why"];
	        this.nextHint = source["nextHint"];
	        this.round = source["round"];
	        this.toolKey = source["toolKey"];
	        this.callSeq = source["callSeq"];
	        this.resultSeq = source["resultSeq"];
	        this.waitNote = source["waitNote"];
	    }
	}

}

export namespace main {
	
	export class CollectResult {
	    total: number;
	    details: Record<string, string>;
	
	    static createFrom(source: any = {}) {
	        return new CollectResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.total = source["total"];
	        this.details = source["details"];
	    }
	}
	export class EventInfo {
	    id: number;
	    seq: number;
	    time: string;
	    kind: string;
	    title: string;
	    content: string;
	    meta: string;
	    parentId: string;
	
	    static createFrom(source: any = {}) {
	        return new EventInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.seq = source["seq"];
	        this.time = source["time"];
	        this.kind = source["kind"];
	        this.title = source["title"];
	        this.content = source["content"];
	        this.meta = source["meta"];
	        this.parentId = source["parentId"];
	    }
	}
	export class HarnessInfo {
	    name: string;
	    sessionCount: number;
	    eventCount: number;
	    lastTime: string;
	
	    static createFrom(source: any = {}) {
	        return new HarnessInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.sessionCount = source["sessionCount"];
	        this.eventCount = source["eventCount"];
	        this.lastTime = source["lastTime"];
	    }
	}
	export class SearchHitInfo {
	    harness: string;
	    sessionId: string;
	    seq: number;
	    time: string;
	    kind: string;
	    title: string;
	    content: string;
	
	    static createFrom(source: any = {}) {
	        return new SearchHitInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.harness = source["harness"];
	        this.sessionId = source["sessionId"];
	        this.seq = source["seq"];
	        this.time = source["time"];
	        this.kind = source["kind"];
	        this.title = source["title"];
	        this.content = source["content"];
	    }
	}
	export class SessionInfo {
	    sessionId: string;
	    eventCount: number;
	    firstTime: string;
	    lastTime: string;
	    title: string;
	    pinned: boolean;
	
	    static createFrom(source: any = {}) {
	        return new SessionInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.sessionId = source["sessionId"];
	        this.eventCount = source["eventCount"];
	        this.firstTime = source["firstTime"];
	        this.lastTime = source["lastTime"];
	        this.title = source["title"];
	        this.pinned = source["pinned"];
	    }
	}

}

export namespace usage {
	
	export class ModelUsage {
	    model: string;
	    records: number;
	    inputTokens: number;
	    outputTokens: number;
	    cacheReadTokens: number;
	    cacheWriteTokens: number;
	    reasoningTokens: number;
	    totalTokens: number;
	    costUsd: number;
	
	    static createFrom(source: any = {}) {
	        return new ModelUsage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.model = source["model"];
	        this.records = source["records"];
	        this.inputTokens = source["inputTokens"];
	        this.outputTokens = source["outputTokens"];
	        this.cacheReadTokens = source["cacheReadTokens"];
	        this.cacheWriteTokens = source["cacheWriteTokens"];
	        this.reasoningTokens = source["reasoningTokens"];
	        this.totalTokens = source["totalTokens"];
	        this.costUsd = source["costUsd"];
	    }
	}
	export class Summary {
	    records: number;
	    skipped: number;
	    inputTokens: number;
	    outputTokens: number;
	    cacheReadTokens: number;
	    cacheWriteTokens: number;
	    reasoningTokens: number;
	    totalTokens: number;
	    costUsd: number;
	    costKnown: boolean;
	    failedCalls: number;
	    models: ModelUsage[];
	    cacheInTotal: boolean;
	    inputIncludesCache: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Summary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.records = source["records"];
	        this.skipped = source["skipped"];
	        this.inputTokens = source["inputTokens"];
	        this.outputTokens = source["outputTokens"];
	        this.cacheReadTokens = source["cacheReadTokens"];
	        this.cacheWriteTokens = source["cacheWriteTokens"];
	        this.reasoningTokens = source["reasoningTokens"];
	        this.totalTokens = source["totalTokens"];
	        this.costUsd = source["costUsd"];
	        this.costKnown = source["costKnown"];
	        this.failedCalls = source["failedCalls"];
	        this.models = this.convertValues(source["models"], ModelUsage);
	        this.cacheInTotal = source["cacheInTotal"];
	        this.inputIncludesCache = source["inputIncludesCache"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

