// Sends session commands to the coordinator through the UI's command inbox.
// The layout loads this script and writes the per-process command token in
// <meta name="factory-csrf-token"> when the session command API is on.
//
// window.factoryCommands.submit(command) posts one command and resolves with
// the final answer. A command is {session_id, kind, expected_revision,
// payload} and may name request_id; otherwise a new one is chosen. A lost
// answer is retried with the same request ID, so the coordinator applies the
// command at most once. An accepted command is not complete: submit polls
// the command until the coordinator records its outcome. submit rejects only
// when the UI server could not be reached; window.factoryCommands.isAnswer
// tells such a transport failure from a server answer, so a caller keeps its
// request ID for a replay until the server answered.
//
// A <form data-session-command="KIND" data-session-id="ID"
// data-expected-revision="N"> submits its named fields as the payload.
// A field with data-type="number" sends a number, a checkbox sends a
// boolean, and checkboxes with data-list send the list of their checked
// values. The form is marked data-dirty while it has unsent input, which
// pauses the automatic page refresh, so a refresh never wipes typed input.
// The outcome is written into the form's [data-command-status] element, and
// a "factory:command" event with the answer is sent from the form. When the
// UI server cannot be reached, a "factory:command-unreachable" event is sent
// instead and the form keeps its request ID for the resend. A form also keeps
// its request ID after an answer that is not final.
(function () {
  "use strict";

  var meta = document.querySelector('meta[name="factory-csrf-token"]');
  if (!meta) {
    return;
  }
  var token = meta.getAttribute("content");
  var pollMilliseconds = 1000;
  var maxSendAttempts = 3;

  // newRequestID returns a random request ID the store accepts.
  function newRequestID() {
    if (window.crypto && window.crypto.randomUUID) {
      return window.crypto.randomUUID();
    }
    var bytes = new Uint8Array(16);
    window.crypto.getRandomValues(bytes);
    return Array.prototype.map.call(bytes, function (b) {
      return ("0" + b.toString(16)).slice(-2);
    }).join("");
  }

  // isAnswer reports whether value is an answer of the UI server, which
  // always carries its HTTP status. A transport failure, for example the
  // TypeError of a failed fetch, is not an answer.
  function isAnswer(value) {
    return !!value && typeof value === "object" && typeof value.status === "number";
  }

  // isFinal reports whether answer settles its request: a completed command,
  // or a refusal that appended nothing. A store_unavailable answer is not
  // final, because the command can be in the inbox; a resend with the same
  // request ID then returns it.
  function isFinal(answer) {
    if (!isAnswer(answer)) {
      return false;
    }
    if (answer.state) {
      return answer.state === "completed";
    }
    return answer.outcome !== "store_unavailable";
  }

  // pending reports whether answer is a command the coordinator has not
  // completed yet.
  function pending(answer) {
    return answer.state === "accepted" || answer.state === "applying";
  }

  // readAnswer returns the JSON answer with its HTTP status.
  function readAnswer(response) {
    return response.json().then(function (answer) {
      answer.status = response.status;
      return answer;
    });
  }

  // send posts the command once. A network failure is retried with the same
  // request ID; an HTTP answer is final.
  function send(command, attempt) {
    return fetch("/api/commands", {
      method: "POST",
      credentials: "same-origin",
      cache: "no-store",
      headers: { "Content-Type": "application/json", "X-Factory-CSRF": token },
      body: JSON.stringify(command)
    }).then(readAnswer, function (error) {
      if (attempt >= maxSendAttempts) {
        throw error;
      }
      return send(command, attempt + 1);
    });
  }

  // wait polls an accepted command until it completes. A poll answer
  // without a command state, or a failed poll, keeps the last known state
  // and polls again.
  function wait(answer) {
    if (!pending(answer)) {
      return Promise.resolve(answer);
    }
    return new Promise(function (resolve) {
      window.setTimeout(resolve, pollMilliseconds);
    }).then(function () {
      return fetch("/api/commands/" + encodeURIComponent(answer.request_id), {
        credentials: "same-origin",
        cache: "no-store"
      }).then(readAnswer).then(function (polled) {
        return polled.state ? polled : answer;
      }, function () {
        return answer;
      });
    }).then(wait);
  }

  // submit posts command and resolves with its final answer.
  function submit(command) {
    command = Object.assign({ payload: {} }, command);
    if (!command.request_id) {
      command.request_id = newRequestID();
    }
    return send(command, 1).then(wait);
  }

  // describe returns a short readable text for one answer.
  function describe(answer) {
    var text = answer.outcome || answer.state || "unknown";
    return answer.detail ? text + ": " + answer.detail : text;
  }

  // payloadOf reads the named fields of form.
  function payloadOf(form) {
    var payload = {};
    Array.prototype.forEach.call(form.elements, function (field) {
      if (!field.name || field.disabled) {
        return;
      }
      if (field.type === "checkbox" && "list" in field.dataset) {
        payload[field.name] = payload[field.name] || [];
        if (field.checked) {
          payload[field.name].push(field.value);
        }
      } else if (field.type === "checkbox") {
        payload[field.name] = field.checked;
      } else if (field.dataset.type === "number") {
        payload[field.name] = Number(field.value);
      } else {
        payload[field.name] = field.value;
      }
    });
    return payload;
  }

  // touch records a changed draft independently of a pending command's bytes.
  function touch(form) {
    form.dataset.dirty = "true";
    form.dataset.editVersion = String(Number(form.dataset.editVersion || 0) + 1);
  }

  // editing includes in-flight commands and typing that began during a read.
  function editing(submitted) {
    var dirty = Array.prototype.some.call(document.querySelectorAll("form[data-dirty], form[data-command-pending]"), function (form) {
      return form.dataset.dirty || form.dataset.commandPending;
    });
    var active = document.activeElement;
    var focused = active && active.closest && active.closest("form");
    return Boolean(dirty || focused && focused !== submitted);
  }

  // refresh reveals new state only after every unsent form is safe to replace.
  function refresh(submitted, destination) {
    if (editing(submitted)) {
      var notice = document.getElementById("revision-status");
      if (notice) { notice.hidden = false; }
      return false;
    }
    if (destination) { window.location.href = destination; }
    else { window.location.reload(); }
    return true;
  }

  // invalidatePlan withdraws old preview actions as soon as draft edits exist.
  function invalidatePlan() {
    Array.prototype.forEach.call(document.querySelectorAll("[data-plan-approval-action] button"), function (control) {
      control.disabled = true;
      control.dataset.draftBlocked = "true";
    });
    var notice = document.getElementById("plan-unsaved");
    if (notice) { notice.hidden = false; }
  }

  // wire connects one command form.
  function wire(form) {
    if (form.dataset.commandWired) {
      return;
    }
    form.dataset.commandWired = "true";
    form.addEventListener("input", function () {
      touch(form);
    });
    form.addEventListener("submit", function (event) {
      event.preventDefault();
      var status = form.querySelector("[data-command-status]");
      if (status) {
        status.textContent = "sending";
      }
      // The form keeps its request ID until an answer arrives, so a submit
      // after a lost answer repeats the same request.
      if (!form.dataset.requestId) {
        form.dataset.requestId = newRequestID();
      }
      if (!form.factoryPendingCommand) {
        var command = { request_id: form.dataset.requestId, kind: form.dataset.sessionCommand,
          expected_revision: Number(form.dataset.expectedRevision), payload: payloadOf(form) };
        if (form.dataset.commandScope) { command.scope = form.dataset.commandScope; command.subject_id = form.dataset.subjectId; }
        else { command.session_id = form.dataset.sessionId; }
        form.factoryPendingCommand = command;
        form.factorySubmittedVersion = form.dataset.editVersion || "0";
      }
      form.dataset.commandPending = "true";
      send(form.factoryPendingCommand, 1).then(function (answer) {
        if (status && pending(answer)) {
          status.textContent = "accepted; waits for the coordinator";
        }
        return wait(answer);
      }).then(function (answer) {
        if (status) {
          status.textContent = describe(answer) + (isFinal(answer) ? "" : "; submit again to retry the same request");
        }
        delete form.dataset.commandPending;
        if (isFinal(answer)) {
          if (answer.outcome === "applied" && (form.dataset.editVersion || "0") === form.factorySubmittedVersion) { delete form.dataset.dirty; }
          else { form.dataset.dirty = "true"; }
          delete form.dataset.requestId;
          delete form.factoryPendingCommand;
        } else { form.dataset.dirty = "true"; }
        form.dispatchEvent(new CustomEvent("factory:command", { bubbles: true, detail: answer }));
      }, function () {
        delete form.dataset.commandPending;
        form.dataset.dirty = "true";
        if (status) {
          status.textContent = "UI server unreachable: submit again to retry the same request";
        }
        form.dispatchEvent(new CustomEvent("factory:command-unreachable", { bubbles: true }));
      });
    });
  }

  // wireAll connects every command form on the page, also after a refresh
  // swapped the content.
  function wireAll() {
    Array.prototype.forEach.call(document.querySelectorAll("form[data-session-command]"), wire);
  }

  document.addEventListener("factory:command", function (event) {
    if (event.detail && event.detail.outcome === "applied") { refresh(event.target); }
  });
  window.factoryCommands = { submit: submit, newRequestID: newRequestID, isAnswer: isAnswer, isFinal: isFinal,
    refresh: refresh, editing: editing, touch: touch, invalidatePlan: invalidatePlan };
  wireAll();
  new MutationObserver(wireAll).observe(document.body, { childList: true, subtree: true });
})();
