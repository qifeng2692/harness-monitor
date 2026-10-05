export namespace explain {
	
	export class StepExplain {
	    summary: string;
	    from: string;
	    to: string;
	    visible: string;
	    why: string;
	    nextHint: string;
	
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
	    }
	}

}

