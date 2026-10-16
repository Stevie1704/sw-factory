// Edits the plan draft on the plan review page and guards its command forms.
// The page writes the current draft as JSON in
// <script type="application/json" id="plan-draft-data"> and an empty
// [data-plan-editor] element; this script builds the editor in it.
//
// The editor keeps a copy of the draft. Removing or splitting an item marks
// every reference to it that needs a decision: a removed parent or
// dependency, or a split item that the reference must now name. The draft
// cannot be saved until each marked reference is repaired. Save sends the
// complete plan as one edit_draft command through window.factoryCommands;
// the coordinator validates it again and saves the next plan revision.
//
// A form with data-single-submit disables its button while its command is
// open, so a second click cannot send a second request. A lost answer
// enables it again, so the user can resend the same request. Every command that
// the coordinator applied reloads the page, so the page shows the new state.
(function () {
  "use strict";

  // A page refresh can insert this script again; one copy is enough.
  if (window.factoryPlanEditor) {
    return;
  }
  window.factoryPlanEditor = true;

  // byId returns the node with id from nodes.
  function byId(nodes, id) {
    for (var i = 0; i < nodes.length; i++) {
      if (nodes[i].id === id) {
        return nodes[i];
      }
    }
    return null;
  }

  // element creates a tag with optional text.
  function element(tag, text) {
    var created = document.createElement(tag);
    if (text !== undefined) {
      created.textContent = text;
    }
    return created;
  }

  // button creates a non-submitting button with an action.
  function button(text, action) {
    var created = element("button", text);
    created.type = "button";
    created.dataset.action = action;
    return created;
  }

  // labelled wraps a field in a label.
  function labelled(text, field) {
    var label = element("label", text + " ");
    label.appendChild(field);
    return label;
  }

  // copyNode returns an editable copy of a draft node.
  function copyNode(node) {
    return {
      id: node.id,
      kind: node.kind,
      title: node.title,
      body: node.body,
      acceptance_criteria: (node.acceptance_criteria || []).slice(),
      parent_id: node.parent_id || "",
      depends_on: (node.depends_on || []).slice(),
      brief_refs: (node.brief_refs || []).slice(),
      repairs: []
    };
  }

  // Editor is one plan editor bound to one draft.
  function Editor(root, draft) {
    this.root = root;
    this.draft = draft;
    this.nodes = draft.nodes.map(copyNode);
    // requestID is kept after a lost answer, so the next Save replays it;
    // inFlight is set only while a request waits for its answer.
    this.requestID = "";
    this.inFlight = false;
    this.form = element("form");
    this.form.className = "plan-edit-form";
    this.form.noValidate = true;
    root.appendChild(this.form);
    this.form.addEventListener("input", this.onInput.bind(this));
    this.form.addEventListener("click", this.onClick.bind(this));
    this.form.addEventListener("submit", this.onSubmit.bind(this));
    this.render();
  }

  // markDirty pauses the automatic refresh until the draft is saved.
  Editor.prototype.markDirty = function () {
    this.form.dataset.dirty = "true";
  };

  // onInput copies one edited field into the draft.
  Editor.prototype.onInput = function (event) {
    var field = event.target;
    var holder = field.closest("[data-edit-node]");
    if (!holder || !field.dataset.field) {
      return;
    }
    var node = byId(this.nodes, holder.dataset.editNode);
    var name = field.dataset.field;
    if (name === "acceptance_criteria") {
      node.acceptance_criteria = field.value.split("\n").map(function (line) {
        return line.trim();
      }).filter(Boolean);
    } else if (name === "depends_on") {
      var target = field.value;
      node.depends_on = node.depends_on.filter(function (id) {
        return id !== target;
      });
      if (field.checked) {
        node.depends_on.push(target);
      }
    } else {
      node[name] = field.value;
    }
    this.markDirty();
    this.showState();
  };

  // onClick runs the editor buttons.
  Editor.prototype.onClick = function (event) {
    var control = event.target.closest("button[data-action]");
    if (!control) {
      return;
    }
    var holder = control.closest("[data-edit-node]");
    var id = holder ? holder.dataset.editNode : "";
    switch (control.dataset.action) {
      case "remove":
        this.remove(id);
        break;
      case "split":
        this.split(id);
        break;
      case "repair":
        this.repair(id, Number(control.dataset.repair));
        break;
      case "discard":
        window.location.reload();
        return;
      default:
        return;
    }
    this.markDirty();
    this.render();
  };

  // remove deletes node id and marks every reference to it for repair.
  Editor.prototype.remove = function (id) {
    this.nodes = this.nodes.filter(function (node) {
      return node.id !== id;
    });
    var others = this.nodes.map(function (node) {
      return node.id;
    });
    this.nodes.forEach(function (node) {
      node.repairs = node.repairs.filter(function (repair) {
        return repair.from !== id;
      });
      node.repairs.forEach(function (repair) {
        repair.choices = repair.choices.filter(function (choice) {
          return choice.ids.indexOf(id) < 0;
        });
      });
      var candidates = others.filter(function (other) {
        return other !== node.id;
      });
      if (node.parent_id === id) {
        node.repairs.push({
          field: "parent_id", from: id,
          reason: "Parent " + id + " was removed. Choose a new parent or none.",
          choices: [{ label: "No parent", ids: [] }].concat(candidates.map(function (other) {
            return { label: "Part of " + other, ids: [other] };
          }))
        });
      }
      if (node.depends_on.indexOf(id) >= 0) {
        node.repairs.push({
          field: "depends_on", from: id,
          reason: "Dependency " + id + " was removed. Drop it or depend on another item.",
          choices: [{ label: "Drop the dependency", ids: [] }].concat(candidates.map(function (other) {
            return { label: "Depend on " + other, ids: [other] };
          }))
        });
      }
    });
  };

  // freeID returns an unused node ID derived from id.
  Editor.prototype.freeID = function (id) {
    for (var n = 2; ; n++) {
      var candidate = id + "-" + n;
      if (!byId(this.nodes, candidate)) {
        return candidate;
      }
    }
  };

  // split adds a second part after node id, moves the second half of its
  // acceptance criteria there, and marks every reference to id for a choice
  // between the parts.
  Editor.prototype.split = function (id) {
    var original = byId(this.nodes, id);
    var part = copyNode(original);
    part.id = this.freeID(id);
    part.title = original.title + " (part 2)";
    var criteria = original.acceptance_criteria;
    if (criteria.length > 1) {
      var half = Math.ceil(criteria.length / 2);
      original.acceptance_criteria = criteria.slice(0, half);
      part.acceptance_criteria = criteria.slice(half);
    }
    this.nodes.splice(this.nodes.indexOf(original) + 1, 0, part);
    this.nodes.forEach(function (node) {
      if (node === part || node === original) {
        return;
      }
      if (node.parent_id === id) {
        node.repairs.push({
          field: "parent_id", from: id,
          reason: id + " was split into " + id + " and " + part.id + ". Choose the parent.",
          choices: [{ label: "Part of " + id, ids: [id] }, { label: "Part of " + part.id, ids: [part.id] }]
        });
      }
      if (node.depends_on.indexOf(id) >= 0) {
        node.repairs.push({
          field: "depends_on", from: id,
          reason: id + " was split into " + id + " and " + part.id + ". Choose what this item depends on.",
          choices: [
            { label: "Depend on " + id, ids: [id] },
            { label: "Depend on " + part.id, ids: [part.id] },
            { label: "Depend on both", ids: [id, part.id] }
          ]
        });
      }
    });
  };

  // repair applies the selected choice of one marked reference.
  Editor.prototype.repair = function (id, index) {
    var node = byId(this.nodes, id);
    var repair = node.repairs[index];
    var select = this.form.querySelector('[data-edit-node="' + id + '"] select[data-repair-choice="' + index + '"]');
    var choice = repair.choices[Number(select.value)];
    if (repair.field === "parent_id") {
      node.parent_id = choice.ids.length ? choice.ids[0] : "";
    } else {
      node.depends_on = node.depends_on.filter(function (dependency) {
        return dependency !== repair.from;
      });
      choice.ids.forEach(function (dependency) {
        if (node.depends_on.indexOf(dependency) < 0) {
          node.depends_on.push(dependency);
        }
      });
    }
    node.repairs.splice(index, 1);
  };

  // openRepairs counts the references that still need a decision.
  Editor.prototype.openRepairs = function () {
    return this.nodes.reduce(function (count, node) {
      return count + node.repairs.length;
    }, 0);
  };

  // render rebuilds the editor from the draft.
  Editor.prototype.render = function () {
    var form = this.form;
    var nodes = this.nodes;
    var kinds = this.draft.kinds;
    form.textContent = "";
    nodes.forEach(function (node) {
      form.appendChild(renderNode(node, nodes, kinds));
    });
    var actions = element("p");
    actions.className = "plan-edit-actions";
    this.save = element("button", "Save as the next plan revision");
    this.save.type = "submit";
    actions.appendChild(this.save);
    actions.appendChild(document.createTextNode(" "));
    actions.appendChild(button("Discard changes", "discard"));
    this.status = element("output");
    this.status.setAttribute("role", "status");
    actions.appendChild(document.createTextNode(" "));
    actions.appendChild(this.status);
    form.appendChild(actions);
    this.showState();
  };

  // showState enables save only when no reference waits for a repair and no
  // request is in flight. A request ID kept for a replay does not disable it.
  Editor.prototype.showState = function () {
    var open = this.openRepairs();
    this.save.disabled = open > 0 || this.nodes.length === 0 || this.inFlight;
    if (open > 0) {
      this.status.textContent = open + " reference(s) need a repair before you can save.";
    } else if (this.nodes.length === 0) {
      this.status.textContent = "A plan needs at least one item.";
    } else if (this.requestID === "") {
      this.status.textContent = "";
    }
  };

  // onSubmit sends the complete draft as one edit_draft command.
  Editor.prototype.onSubmit = function (event) {
    event.preventDefault();
    if (this.openRepairs() > 0 || this.inFlight || !window.factoryCommands) {
      return;
    }
    // The request ID stays until an answer arrives, so a resend after a
    // lost answer repeats the same request.
    if (!this.requestID) {
      this.requestID = window.factoryCommands.newRequestID();
    }
    var editor = this;
    editor.inFlight = true;
    editor.showState();
    editor.status.textContent = "sending";
    window.factoryCommands.submit({
      request_id: editor.requestID,
      session_id: editor.draft.session_id,
      kind: "edit_draft",
      expected_revision: editor.draft.expected_revision,
      payload: { nodes: editor.nodes.map(payloadNode) }
    }).then(function (answer) {
      editor.inFlight = false;
      editor.requestID = "";
      if (answer.outcome === "applied") {
        delete editor.form.dataset.dirty;
        window.location.reload();
        return;
      }
      editor.showState();
      editor.status.textContent = (answer.outcome || answer.state) + ": " + (answer.detail || "") +
        (answer.outcome === "conflict" ? " Your edits stay here; copy what you need, then discard and edit again." : "");
    }, function () {
      editor.inFlight = false;
      editor.showState();
      editor.status.textContent = "UI server unreachable: save again to retry the same request";
    });
  };

  // payloadNode returns the command shape of one draft node.
  function payloadNode(node) {
    return {
      id: node.id,
      kind: node.kind,
      title: node.title,
      body: node.body,
      acceptance_criteria: node.acceptance_criteria,
      parent_id: node.parent_id,
      depends_on: node.depends_on,
      brief_refs: node.brief_refs
    };
  }

  // renderNode builds the fieldset of one node.
  function renderNode(node, nodes, kinds) {
    var set = element("fieldset");
    set.className = "edit-node";
    set.dataset.editNode = node.id;
    set.appendChild(element("legend", node.id));

    var kind = element("select");
    kind.dataset.field = "kind";
    kinds.forEach(function (value) {
      var option = element("option", value);
      option.value = value;
      option.selected = value === node.kind;
      kind.appendChild(option);
    });
    set.appendChild(labelled("Kind", kind));

    var title = element("input");
    title.dataset.field = "title";
    title.value = node.title;
    set.appendChild(labelled("Title", title));

    var body = element("textarea");
    body.dataset.field = "body";
    body.rows = 4;
    body.value = node.body;
    set.appendChild(labelled("Body", body));

    var criteria = element("textarea");
    criteria.dataset.field = "acceptance_criteria";
    criteria.rows = 3;
    criteria.value = node.acceptance_criteria.join("\n");
    set.appendChild(labelled("Acceptance criteria, one per line", criteria));

    var others = nodes.filter(function (other) {
      return other.id !== node.id;
    });
    var parent = element("select");
    parent.dataset.field = "parent_id";
    [{ id: "", label: "No parent" }].concat(others.map(function (other) {
      return { id: other.id, label: other.id + " " + other.title };
    })).forEach(function (choice) {
      var option = element("option", choice.label);
      option.value = choice.id;
      option.selected = choice.id === node.parent_id;
      parent.appendChild(option);
    });
    parent.disabled = node.repairs.some(function (repair) {
      return repair.field === "parent_id";
    });
    set.appendChild(labelled("Parent", parent));

    var depends = element("fieldset");
    depends.className = "edit-dependencies";
    depends.appendChild(element("legend", "Depends on"));
    others.forEach(function (other) {
      var box = element("input");
      box.type = "checkbox";
      box.dataset.field = "depends_on";
      box.value = other.id;
      box.checked = node.depends_on.indexOf(other.id) >= 0;
      depends.appendChild(labelled(other.id, box));
    });
    set.appendChild(depends);

    node.repairs.forEach(function (repair, index) {
      set.appendChild(renderRepair(repair, index));
    });

    var actions = element("p");
    actions.appendChild(button("Split this item", "split"));
    actions.appendChild(document.createTextNode(" "));
    actions.appendChild(button("Remove this item", "remove"));
    set.appendChild(actions);
    return set;
  }

  // renderRepair builds the decision for one marked reference.
  function renderRepair(repair, index) {
    var box = element("div");
    box.className = "notice notice-alarm repair";
    box.setAttribute("role", "alert");
    box.appendChild(element("p", repair.reason));
    var select = element("select");
    select.dataset.repairChoice = String(index);
    repair.choices.forEach(function (choice, position) {
      var option = element("option", choice.label);
      option.value = String(position);
      select.appendChild(option);
    });
    box.appendChild(labelled("Repair", select));
    var apply = button("Apply", "repair");
    apply.dataset.repair = String(index);
    box.appendChild(document.createTextNode(" "));
    box.appendChild(apply);
    return box;
  }

  // startEditors builds an editor in every new editor element, also after a
  // refresh swapped the page content.
  function startEditors() {
    Array.prototype.forEach.call(document.querySelectorAll("[data-plan-editor]"), function (root) {
      if (root.dataset.ready) {
        return;
      }
      var data = document.getElementById("plan-draft-data");
      if (!data) {
        return;
      }
      root.dataset.ready = "true";
      try {
        new Editor(root, JSON.parse(data.textContent));
      } catch (error) {
        root.textContent = "The editor could not read the draft: " + error.message;
      }
    });
  }

  // guardSingleSubmit disables the button of a single-submit form while its
  // command is open, and moves focus away so the page refresh can show
  // progress.
  document.addEventListener("submit", function (event) {
    var form = event.target;
    if (!form.matches || !form.matches("form[data-single-submit]")) {
      return;
    }
    Array.prototype.forEach.call(form.querySelectorAll("button"), function (control) {
      control.disabled = true;
    });
    if (document.activeElement && document.activeElement.blur) {
      document.activeElement.blur();
    }
  }, true);

  // enableButtons enables every button of form again.
  function enableButtons(form) {
    Array.prototype.forEach.call(form.querySelectorAll("button"), function (control) {
      control.disabled = false;
    });
  }

  // An applied command changed the session, so the page reads it again. Any
  // other answer re-enables the form; its status shows the reason.
  document.addEventListener("factory:command", function (event) {
    var answer = event.detail || {};
    if (answer.outcome === "applied") {
      window.location.reload();
      return;
    }
    enableButtons(event.target);
  });

  // A lost answer re-enables the form, so a resend repeats the same request.
  document.addEventListener("factory:command-unreachable", function (event) {
    enableButtons(event.target);
  });

  startEditors();
  new MutationObserver(startEditors).observe(document.body, { childList: true, subtree: true });
})();
